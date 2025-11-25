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

    "github.com/rs/zerolog/log"
    "github.com/bilal/switchify-agent/internal/config"
    "github.com/google/uuid"
)

// Communicator sends telemetry to backend with retries and buffering.
type Communicator struct {
    cfg         *config.Config
    endpoint    string
    client      *http.Client
    token       string
    queue       chan Telemetry
    wg          sync.WaitGroup
    sendInterval time.Duration
    maxQueue    int
    ctx         context.Context
    cancel      context.CancelFunc
}

// New creates communicator; it does NOT start the send loop.
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

    maxQ := cfg.Agent.MaxQueueSize
    if maxQ <= 0 {
        maxQ = 1000
    }

    ctx, cancel := context.WithCancel(context.Background())
    return &Communicator{
        cfg:         cfg,
        endpoint:    cfg.Agent.BackendURL,
        client:      client,
        token:       token,
        queue:       make(chan Telemetry, maxQ),
        sendInterval: time.Duration(cfg.Agent.SendIntervalSeconds) * time.Second,
        maxQueue:    maxQ,
        ctx:         ctx,
        cancel:      cancel,
    }
}

// Start background sender loop. Call once.
func (c *Communicator) Start() {
    c.wg.Add(1)
    go c.loop()
    log.Info().Int("queue_capacity", c.maxQueue).Msg("communicator started")
}

// Shutdown stops sender and waits for queued items to be processed (with timeout context).
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

// Send enqueues a telemetry item. Non-blocking: if queue full, it drops oldest item.
func (c *Communicator) Send(t Telemetry) {
    // ensure correlation id
    if t.CorrelationID == "" {
        t.CorrelationID = uuid.New().String()
    }

    select {
    case c.queue <- t:
        // enqueued
    default:
        // queue full: drop oldest (read one) then enqueue
        select {
        case <-c.queue:
        default:
        }
        select {
        case c.queue <- t:
        default:
            // if still fails, drop and log
            log.Warn().Msg("telemetry dropped: queue full")
        }
    }
}

// loop batches and sends
func (c *Communicator) loop() {
    defer c.wg.Done()

    ticker := time.NewTicker(c.sendInterval)
    defer ticker.Stop()

    // local buffer
    buffer := make([]Telemetry, 0, 128)

    for {
        select {
        case <-c.ctx.Done():
            // flush remaining
            for {
                select {
                case t := <-c.queue:
                    buffer = append(buffer, t)
                default:
                    if len(buffer) > 0 {
                        c.flushWithRetry(buffer)
                    }
                    return
                }
            }

        case t := <-c.queue:
            buffer = append(buffer, t)
            if len(buffer) >= 100 {
                c.flushWithRetry(buffer)
                buffer = buffer[:0]
            }

        case <-ticker.C:
            if len(buffer) > 0 {
                c.flushWithRetry(buffer)
                buffer = buffer[:0]
            }
        }
    }
}

// flushWithRetry posts payload and retries with exponential backoff + jitter
func (c *Communicator) flushWithRetry(items []Telemetry) {
    // batch payload
    payload, err := json.Marshal(items)
    if err != nil {
        log.Error().Err(err).Msg("marshal telemetry failed")
        return
    }

    maxAttempts := 6
    baseDelay := 500 * time.Millisecond

    var attempt int
    for {
        attempt++
        req, err := http.NewRequestWithContext(c.ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
        if err != nil {
            log.Error().Err(err).Msg("create request failed")
            return
        }
        req.Header.Set("Content-Type", "application/json")
        if c.token != "" {
            req.Header.Set("Authorization", "Bearer "+c.token)
        }
        // add correlation header for the batch (use first item correlation id)
        if len(items) > 0 {
            req.Header.Set("X-Correlation-ID", items[0].CorrelationID)
        }

        resp, err := c.client.Do(req)
        if err == nil {
            // read and close body to reuse connection
            if resp.Body != nil {
                resp.Body.Close()
            }
            if resp.StatusCode >= 200 && resp.StatusCode < 300 {
                log.Info().Int("count", len(items)).Str("correlation", req.Header.Get("X-Correlation-ID")).Msg("telemetry posted")
                return
            }
            // server error: treat as retryable
            err = errors.New(fmt.Sprintf("bad status: %d", resp.StatusCode))
        }

        // error path: log
        log.Warn().Err(err).Int("attempt", attempt).Int("count", len(items)).Msg("telemetry post failed, will retry")

        if attempt >= maxAttempts {
            log.Error().Int("attempts", attempt).Msg("max attempts reached, dropping telemetry batch")
            return
        }

        // exponential backoff with jitter
        backoff := time.Duration(math.Pow(2, float64(attempt-1))) * baseDelay
        jitter := time.Duration(rand.Int63n(int64(baseDelay)))
        sleep := backoff + jitter

        select {
        case <-time.After(sleep):
            // next attempt
        case <-c.ctx.Done():
            log.Warn().Msg("communicator context cancelled during backoff")
            return
        }
    }
}
