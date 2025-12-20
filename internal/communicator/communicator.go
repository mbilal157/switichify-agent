package communicator

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/bilal/switchify-agent/internal/config"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

const (
	maxBatchSize = 100
)

// ---------- Payload Types ----------

type MetricsPayload struct {
	Level      string `json:"level"`
	IspState   string `json:"isp_state"`
	LatencyMs  int    `json:"latency_ms"`
	PacketLoss int    `json:"packet_loss"`
	JitterMs   int    `json:"jitter_ms"`
	Time       string `json:"time"`
	Message    string `json:"message"`

	CorrelationID string `json:"-"`
}

type LogPayload struct {
	Level   string `json:"level"`
	Time    string `json:"time"`
	Message string `json:"message"`

	Error   string `json:"error,omitempty"`
	Attempt int    `json:"attempt,omitempty"`
	Count   int    `json:"count,omitempty"`

	CorrelationID string `json:"-"`
}

// ---------- Communicator ----------

type Communicator struct {
	cfg *config.Config

	client *http.Client
	token  string

	metricsURL string
	logsURL    string

	metricsQueue chan MetricsPayload
	logsQueue    chan LogPayload

	sendInterval time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// ---------- Constructor ----------

func New(cfg *config.Config) *Communicator {
	tlsCfg := &tls.Config{
		InsecureSkipVerify: cfg.Agent.InsecureSkipVerify,
	}

	client := &http.Client{
		Timeout: time.Duration(cfg.Agent.TimeoutSeconds) * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: tlsCfg,
		},
	}

	token := ""
	if cfg.Agent.BackendAuthTokenEnv != "" {
		token = os.Getenv(cfg.Agent.BackendAuthTokenEnv)
	}

	queueSize := cfg.Agent.MaxQueueSize
	if queueSize <= 0 {
		queueSize = 1000
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &Communicator{
		cfg: cfg,

		client: client,
		token:  token,

		metricsURL: cfg.Agent.BackendURL + "/telemetry/metrics",
		logsURL:    cfg.Agent.BackendURL + "/telemetry/logs",

		metricsQueue: make(chan MetricsPayload, queueSize),
		logsQueue:    make(chan LogPayload, queueSize),

		sendInterval: time.Duration(cfg.Agent.SendIntervalSeconds) * time.Second,

		ctx:    ctx,
		cancel: cancel,
	}
}

// ---------- Lifecycle ----------

func (c *Communicator) Start() {
	c.wg.Add(2)
	go c.metricsLoop()
	go c.logsLoop()

	log.Info().
		Int("metrics_queue", cap(c.metricsQueue)).
		Int("logs_queue", cap(c.logsQueue)).
		Msg("communicator started")
}

func (c *Communicator) Shutdown(ctx context.Context) {
	log.Info().Msg("communicator shutdown initiated")
	c.cancel()

	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		log.Info().Msg("communicator shutdown complete")
	case <-ctx.Done():
		log.Warn().Msg("communicator shutdown timeout")
	}
}

// ---------- Public API ----------

func (c *Communicator) SendMetrics(m MetricsPayload) {
	if m.CorrelationID == "" {
		m.CorrelationID = uuid.New().String()
	}

	select {
	case c.metricsQueue <- m:
	default:
		log.Warn().Msg("metrics dropped: queue full")
	}
}

func (c *Communicator) SendLog(l LogPayload) {
	if l.CorrelationID == "" {
		l.CorrelationID = uuid.New().String()
	}

	select {
	case c.logsQueue <- l:
	default:
		log.Warn().Msg("log dropped: queue full")
	}
}

// ---------- Loops ----------

func (c *Communicator) metricsLoop() {
	defer c.wg.Done()
	c.runLoop(c.metricsQueue, c.metricsURL)
}

func (c *Communicator) logsLoop() {
	defer c.wg.Done()
	c.runLoop(c.logsQueue, c.logsURL)
}

func[T any] (c *Communicator) runLoop(queue chan T, url string) {
	ticker := time.NewTicker(c.sendInterval)
	defer ticker.Stop()

	buffer := make([]T, 0, maxBatchSize)

	for {
		select {
		case <-c.ctx.Done():
			for {
				select {
				case item := <-queue:
					buffer = append(buffer, item)
				default:
					if len(buffer) > 0 {
						c.flushWithRetry(url, buffer)
					}
					return
				}
			}

		case item := <-queue:
			buffer = append(buffer, item)
			if len(buffer) >= maxBatchSize {
				c.flushWithRetry(url, buffer)
				buffer = buffer[:0]
			}

		case <-ticker.C:
			if len(buffer) > 0 {
				c.flushWithRetry(url, buffer)
				buffer = buffer[:0]
			}
		}
	}
}

// ---------- Sender ----------

func (c *Communicator) flushWithRetry(url string, items any) {
	payload, err := json.Marshal(items)
	if err != nil {
		log.Error().Err(err).Msg("marshal failed")
		return
	}

	const maxAttempts = 6
	baseDelay := 500 * time.Millisecond

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(
			c.ctx,
			http.MethodPost,
			url,
			bytes.NewReader(payload),
		)
		if err != nil {
			log.Error().Err(err).Msg("request creation failed")
			return
		}

		req.Header.Set("Content-Type", "application/json")
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}

		resp, err := c.client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				log.Info().
					Int("count", len(payload)).
					Str("endpoint", url).
					Msg("telemetry posted")
				return
			}
			err = errors.New(fmt.Sprintf("bad status: %d", resp.StatusCode))
		}

		log.Warn().
			Err(err).
			Int("attempt", attempt).
			Str("endpoint", url).
			Msg("telemetry post failed, retrying")

		if attempt == maxAttempts {
			log.Error().
				Int("attempts", attempt).
				Str("endpoint", url).
				Msg("max attempts reached, dropping batch")
			return
		}

		backoff := time.Duration(math.Pow(2, float64(attempt-1))) * baseDelay
		jitter := time.Duration(rand.Int63n(int64(baseDelay)))

		select {
		case <-time.After(backoff + jitter):
		case <-c.ctx.Done():
			return
		}
	}
}
