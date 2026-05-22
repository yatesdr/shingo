package www

import (
	"net/http"
	"time"
)

// statusResponse is the JSON shape returned by GET /status.
//
// On-call diagnostic surface — when the plant calls about a stuck
// order, hitting this URL gives the first-pass answer in seconds
// instead of 30 minutes of log hunting.
//
// kafka_connected and subscribers_wired are LOAD-BEARING — they
// surface the deaf-but-running mode the Kafka reconnect retry
// makes possible (Edge keeps running with an inbound outage
// instead of crashing). Without these fields the operator has no
// visibility into that mode.
type statusResponse struct {
	UptimeSeconds    int64     `json:"uptime_seconds"`
	ProcessStartTime time.Time `json:"process_start_time"`
	KafkaConnected   bool      `json:"kafka_connected"`
	SubscribersWired bool      `json:"subscribers_wired"`
	OutboxDepth      int       `json:"outbox_depth"`
	OutboxDepthError string    `json:"outbox_depth_error,omitempty"`
	StationID        string    `json:"station_id"`
}

// statusEngine is the narrow interface the /status handler needs.
// Defined here rather than in router.go so the handler stays
// loosely coupled to the engine concrete type.
type statusEngine interface {
	Uptime() int64
	StartedAt() time.Time
	KafkaConnected() bool
	SubscribersWired() bool
	CountPendingOutbox() (int, error)
	StationID() string
}

func (h *Handlers) apiStatus(w http.ResponseWriter, r *http.Request) {
	eng, ok := h.orchestration.(statusEngine)
	if !ok {
		http.Error(w, "status not available", http.StatusServiceUnavailable)
		return
	}

	resp := statusResponse{
		UptimeSeconds:    eng.Uptime(),
		ProcessStartTime: eng.StartedAt().UTC(),
		KafkaConnected:   eng.KafkaConnected(),
		SubscribersWired: eng.SubscribersWired(),
		StationID:        eng.StationID(),
	}
	if depth, err := eng.CountPendingOutbox(); err != nil {
		resp.OutboxDepthError = err.Error()
	} else {
		resp.OutboxDepth = depth
	}
	writeJSON(w, resp)
}
