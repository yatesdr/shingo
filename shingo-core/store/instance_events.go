package store

import "time"

// Instance event type constants.
const (
	InstanceEventCreated     = "created"
	InstanceEventMoved       = "moved"
	InstanceEventClaimed     = "claimed"
	InstanceEventUnclaimed   = "unclaimed"
	InstanceEventLoaded      = "loaded"
	InstanceEventDepleted    = "depleted"
	InstanceEventFlagged     = "flagged"
	InstanceEventMaintenance = "maintenance"
	InstanceEventRetired     = "retired"
	InstanceEventTagScanned  = "tag_scanned"
	InstanceEventTagMismatch = "tag_mismatch"
)

type InstanceEvent struct {
	ID         int64     `json:"id"`
	InstanceID int64     `json:"instance_id"`
	EventType  string    `json:"event_type"`
	Detail     string    `json:"detail"`
	Actor      string    `json:"actor"`
	CreatedAt  time.Time `json:"created_at"`
}

func (db *DB) CreateInstanceEvent(evt *InstanceEvent) error {
	_, err := db.Exec(db.Q(`INSERT INTO instance_events (instance_id, event_type, detail, actor) VALUES (?, ?, ?, ?)`),
		evt.InstanceID, evt.EventType, evt.Detail, evt.Actor)
	return err
}

// logInstanceEvent is a fire-and-forget helper for lifecycle event logging.
func (db *DB) logInstanceEvent(instanceID int64, eventType, detail string) {
	_ = db.CreateInstanceEvent(&InstanceEvent{
		InstanceID: instanceID,
		EventType:  eventType,
		Detail:     detail,
		Actor:      "system",
	})
}

func (db *DB) ListInstanceEvents(instanceID int64, limit int) ([]*InstanceEvent, error) {
	rows, err := db.Query(db.Q(`SELECT id, instance_id, event_type, detail, actor, created_at FROM instance_events WHERE instance_id = ? ORDER BY created_at DESC LIMIT ?`), instanceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []*InstanceEvent
	for rows.Next() {
		e := &InstanceEvent{}
		var createdAt any
		if err := rows.Scan(&e.ID, &e.InstanceID, &e.EventType, &e.Detail, &e.Actor, &createdAt); err != nil {
			continue
		}
		e.CreatedAt = parseTime(createdAt)
		events = append(events, e)
	}
	return events, nil
}
