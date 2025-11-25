package monitor

import (
	"context"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/bilal/switchify-agent/internal/config"
	"github.com/bilal/switchify-agent/internal/decision"
	"github.com/bilal/switchify-agent/internal/switcher"
	import "github.com/bilal/switchify-agent/internal/communicator"

)

type Monitor struct {
    cfg      *config.Config
    pingMon  *PingMonitor
    engine   *decision.DecisionEngine
    switcher *switcher.Switcher
	comm *communicator.Communicator
}

func New(cfg *config.Config, comm *communicator.Communicator) *Monitor {
    engine := decision.NewEngine(decision.ThresholdConfig{
        MaxLatencyMs:    cfg.Thresholds.MaxLatencyMs,
        MaxPacketLoss:   cfg.Thresholds.MaxPacketLoss,
        MaxJitterMs:     cfg.Thresholds.MaxJitterMs,
        RecoveryLatency: cfg.Thresholds.RecoveryLatency,
        RecoveryLoss:    cfg.Thresholds.RecoveryLoss,
        RecoveryJitter:  cfg.Thresholds.RecoveryJitter,
        Cooldown:        time.Duration(cfg.Thresholds.CooldownSeconds) * time.Second,
    })

    sw, err := switcher.NewSwitcher(cfg.ISP.PrimaryGateway, cfg.ISP.BackupGateway)
    if err != nil {
        panic("invalid gateway IPs in config: " + err.Error())
    }

    return &Monitor{
        cfg:      cfg,
        pingMon:  NewPingMonitor(cfg.Agent.TargetHost, time.Duration(cfg.Agent.PingIntervalSeconds)*time.Second),
        engine:   engine,
        switcher: sw,
		comm:     comm,
    }
}

func (m *Monitor) Run(ctx context.Context) {
	log.Info().Msg("Monitor started")

	ticker := time.NewTicker(time.Duration(m.cfg.Agent.PingIntervalSeconds) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("Monitor stopping")
			return

		case <-ticker.C:
			go m.pingMon.RunOnce()

			metrics := m.pingMon.GetMetrics()
			// create telemetry payload
tp := communicator.Telemetry{
    AgentName:  m.cfg.Agent.Name,
    Timestamp:  time.Now(),
    ISP:        "primary", // optionally detect which ISP you're testing
    LatencyMs:  metrics.AvgLatencyMs,
    PacketLoss: metrics.PacketLoss,
    JitterMs:   metrics.JitterMs,
}

// non-blocking enqueue
m.comm.Send(tp)

			state := m.engine.Evaluate(metrics)

			log.Info().
				Str("isp_state", string(state)).
				Float64("latency_ms", metrics.AvgLatencyMs).
				Float64("packet_loss", metrics.PacketLoss).
				Float64("jitter_ms", metrics.JitterMs).
				Msg("decision evaluated")

			switch state {
			case decision.FailingOver:
				m.switchToBackup()

			case decision.FailingBack:
				m.switchToPrimary()
			}
		}
	}
}

func (m *Monitor) switchToBackup() {
	log.Warn().Msg("Switching to BACKUP ISP (netlink)")

	if err := m.switcher.SwitchToBackup(); err != nil {
		log.Error().Err(err).Msg("failed to switch to backup")
		return
	}

	if m.switcher.IsUsingGateway(m.switcher.BackupGW) {
		log.Info().Msg("backup route verified")
	}
}

func (m *Monitor) switchToPrimary() {
	log.Warn().Msg("Switching to PRIMARY ISP (netlink)")

	if err := m.switcher.SwitchToPrimary(); err != nil {
		log.Error().Err(err).Msg("failed to switch to primary")
		return
	}

	if m.switcher.IsUsingGateway(m.switcher.PrimaryGW) {
		log.Info().Msg("primary route verified")
	}
}