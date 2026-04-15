package engine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingocore/fleet/simulator"
)

// TestHandleCountGroupTransitionBuildsBroadcastCommand verifies the
// subscriber path end-to-end: given an EventCountGroupTransition, a
// CountGroupCommand envelope is enqueued on the outbox with the
// correct subject, broadcast destination, and payload fields.
func TestHandleCountGroupTransitionBuildsBroadcastCommand(t *testing.T) {
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	ev := CountGroupTransitionEvent{
		Group:             "Crosswalk1",
		Desired:           "on",
		Robots:            []string{"AMR-01", "AMR-02"},
		FailSafeTriggered: false,
		Timestamp:         time.Now(),
	}
	eng.handleCountGroupTransition(ev)

	// Drain the outbox to inspect what was enqueued.
	rows, err := db.ListPendingOutbox(10)
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 outbox row, got %d", len(rows))
	}
	row := rows[0]

	if row.Topic != eng.cfg.Messaging.DispatchTopic {
		t.Errorf("topic = %q, want %q", row.Topic, eng.cfg.Messaging.DispatchTopic)
	}
	if row.StationID != protocol.StationBroadcast {
		t.Errorf("station = %q, want broadcast %q", row.StationID, protocol.StationBroadcast)
	}
	if !strings.Contains(row.MsgType, "countgroup.command") {
		t.Errorf("msgType = %q, want it to contain subject %q", row.MsgType, protocol.SubjectCountGroupCommand)
	}

	// Decode envelope to verify payload fields.
	env := &protocol.Envelope{}
	if err := json.Unmarshal(row.Payload, env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.Type != protocol.TypeData {
		t.Errorf("envelope type = %q, want data", env.Type)
	}

	// The envelope payload for TypeData is a Data struct with subject + body.
	var data protocol.Data
	if err := json.Unmarshal(env.Payload, &data); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	if data.Subject != protocol.SubjectCountGroupCommand {
		t.Errorf("data subject = %q, want %q", data.Subject, protocol.SubjectCountGroupCommand)
	}

	var cmd protocol.CountGroupCommand
	if err := json.Unmarshal(data.Body, &cmd); err != nil {
		t.Fatalf("decode command: %v", err)
	}
	if cmd.Group != "Crosswalk1" {
		t.Errorf("group = %q, want Crosswalk1", cmd.Group)
	}
	if cmd.Desired != "on" {
		t.Errorf("desired = %q, want on", cmd.Desired)
	}
	if cmd.RobotCount != 2 {
		t.Errorf("robot_count = %d, want 2", cmd.RobotCount)
	}
	if len(cmd.Robots) != 2 || cmd.Robots[0] != "AMR-01" || cmd.Robots[1] != "AMR-02" {
		t.Errorf("robots = %v, want [AMR-01 AMR-02]", cmd.Robots)
	}
	if cmd.FailSafeTriggered {
		t.Errorf("fail_safe_triggered should be false on normal transition")
	}
	if cmd.CorrelationID == "" {
		t.Errorf("correlation_id should be non-empty")
	}

	// Audit row should also be present.
	audits, err := db.ListEntityAudit("countgroup", 0)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(audits) == 0 {
		t.Fatalf("expected at least one countgroup audit row")
	}
	found := false
	for _, a := range audits {
		if a.Action == "transition" && strings.Contains(a.NewValue, "Crosswalk1") && strings.Contains(a.NewValue, cmd.CorrelationID) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no audit row matched the transition; got %+v", audits)
	}
}

// TestHandleCountGroupTransitionFailSafeAudit verifies the audit row
// distinguishes a fail-safe triggered transition from a normal one.
func TestHandleCountGroupTransitionFailSafeAudit(t *testing.T) {
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	eng.handleCountGroupTransition(CountGroupTransitionEvent{
		Group:             "Crosswalk1",
		Desired:           "on",
		Robots:            nil, // fail-safe fires without a robot list
		FailSafeTriggered: true,
		Timestamp:         time.Now(),
	})

	audits, err := db.ListEntityAudit("countgroup", 0)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	found := false
	for _, a := range audits {
		if a.Action == "fail_safe_on" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected audit row with action=fail_safe_on; got %+v", audits)
	}
}
