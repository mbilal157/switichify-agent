package communicator

import "time"

// Telemetry is the JSON payload sent to backend.
type Telemetry struct {
    AgentName    string    `json:"agent_name"`
    Timestamp    time.Time `json:"timestamp"`
    ISP          string    `json:"isp,omitempty"`
    LatencyMs    float64   `json:"latency_ms,omitempty"`
    PacketLoss   float64   `json:"packet_loss_pct,omitempty"`
    JitterMs     float64   `json:"jitter_ms,omitempty"`
    CorrelationID string   `json:"correlation_id,omitempty"`
}
