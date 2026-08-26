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
//
// READ outbox_depth AND dead_letters TOGETHER. Depth counts only rows still
// under the retry cap, so during an outage it rises while rows retry and then
// falls as they exhaust — a depth returning to zero reads like recovery and
// can equally mean the backlog was destroyed. Springfield, 2026-08-21: depth
// zero, kafka_connected true, 120 messages permanently lost.
type statusResponse struct {
	UptimeSeconds    int64     `json:"uptime_seconds"`
	ProcessStartTime time.Time `json:"process_start_time"`
	// KafkaConnected reports that a Kafka WRITER EXISTS — NOT that the broker
	// is reachable. Connect() does no I/O, so on Edge this is true from the
	// first connect until shutdown regardless of the network. Use
	// kafka_last_publish_ok for reachability. The field keeps its name and
	// value because renaming a key on an on-call JSON surface breaks whatever
	// is already parsing it.
	KafkaConnected   bool   `json:"kafka_connected"`
	SubscribersWired bool   `json:"subscribers_wired"`
	OutboxDepth      int    `json:"outbox_depth"`
	OutboxDepthError string `json:"outbox_depth_error,omitempty"`
	// DeadLetters are un-sent messages past the retry cap. They will never be
	// sent: the drainer's pending query filters on retries < MaxRetries, so an
	// exhausted row falls out of every later pass. Non-zero means data loss
	// has already happened, not that it might.
	DeadLetters      int    `json:"dead_letters"`
	DeadLettersError string `json:"dead_letters_error,omitempty"`
	// KafkaLastPublishOK is the outcome of the most recent publish attempt —
	// the actual reachability signal. Absent (with a nil timestamp) until
	// something has been published, so a freshly booted Edge does not report a
	// failure it never had.
	KafkaLastPublishOK bool       `json:"kafka_last_publish_ok"`
	KafkaLastPublishAt *time.Time `json:"kafka_last_publish_at,omitempty"`
	StationID          string     `json:"station_id"`
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
	CountDeadLetterOutbox() (int, error)
	KafkaLastPublish() (bool, time.Time, bool)
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
	if dead, err := eng.CountDeadLetterOutbox(); err != nil {
		resp.DeadLettersError = err.Error()
	} else {
		resp.DeadLetters = dead
	}
	if ok, at, ever := eng.KafkaLastPublish(); ever {
		resp.KafkaLastPublishOK = ok
		utc := at.UTC()
		resp.KafkaLastPublishAt = &utc
	}
	writeJSON(w, resp)
}
