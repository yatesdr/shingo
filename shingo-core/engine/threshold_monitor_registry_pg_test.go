//go:build docker

package engine

import (
	"encoding/json"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/store/demands"
	"shingocore/store/messaging"
)

// TestThresholdMonitor_OnRegistryChanges_FiresImmediatelyWhenBelowThreshold
// pins the Springfield 6883 fix: when a demand-registry sync newly adds
// (or raises) a threshold for a payload whose current system UOP is
// already below the new value, the monitor must fire
// LoopBelowThresholdSignal during OnRegistryChanges — not wait for the
// next bin/bucket delta. Before the fix, OnRegistryChanges only rebuilt
// the cache and reset the debounce; a zero-stock payload (no upcoming
// delta) stayed silent until Core restart.
func TestThresholdMonitor_OnRegistryChanges_FiresImmediatelyWhenBelowThreshold(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	const (
		stationID = "station-springfield"
		loader    = "MS-LOADER-1"
		payload   = "P-6883"
	)

	// No bins of this payload exist anywhere — system UOP for the
	// payload is 0. Simulates the Springfield case where the payload's
	// in-loop total is below any positive threshold.
	if _, err := db.SyncDemandRegistry(stationID, []demands.RegistryEntry{{
		StationID:             stationID,
		CoreNodeName:          loader,
		Role:                  protocol.ClaimRoleConsume,
		PayloadCode:           payload,
		ReplenishUOPThreshold: 50,
	}}); err != nil {
		t.Fatalf("seed initial registry: %v", err)
	}

	// Snapshot outbox state pre-OnRegistryChanges so the assertion
	// below distinguishes the new signal from anything the test engine
	// emitted at startup. The 3s startup-sweep gate keeps the sweep
	// out of this test's window, but we belt-and-brace anyway.
	preMsgs, _ := db.ListPendingOutbox(50)
	preCount := countLoopBelowThresholdSignals(preMsgs, stationID)

	// Drive OnRegistryChanges directly with a synthetic change list — the
	// same shape handleClaimSync would produce after a real SyncRegistry
	// returned a non-empty change set. This isolates the immediate-fire
	// behavior without depending on the full ClaimSync path.
	eng.thresholdMonitor.OnRegistryChanges([]demands.RegistryChange{{
		StationID:    stationID,
		CoreNodeName: loader,
		PayloadCode:  payload,
		OldThreshold: 0,
		NewThreshold: 50,
	}})

	// SendDataToEdge is synchronous to the outbox (DB write inside
	// SendDataToEdge), so a single re-read should suffice. Allow a
	// small retry window for the rare CI scheduling jitter.
	deadline := time.Now().Add(2 * time.Second)
	var hit *protocol.LoopBelowThresholdSignal
	for time.Now().Before(deadline) {
		msgs, _ := db.ListPendingOutbox(50)
		if countLoopBelowThresholdSignals(msgs, stationID) > preCount {
			hit = findLoopBelowThresholdSignal(t, msgs, stationID)
			if hit != nil {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if hit == nil {
		msgs, _ := db.ListPendingOutbox(50)
		t.Fatalf("expected immediate LoopBelowThresholdSignal to %s after OnRegistryChanges, outbox=%v",
			stationID, outboxSummary(msgs))
	}
	if hit.PayloadCode != payload {
		t.Errorf("signal PayloadCode = %q, want %q", hit.PayloadCode, payload)
	}
	if hit.CoreNodeName != loader {
		t.Errorf("signal CoreNodeName = %q, want %q", hit.CoreNodeName, loader)
	}
	if hit.Threshold != 50 {
		t.Errorf("signal Threshold = %d, want 50", hit.Threshold)
	}
	if hit.CurrentUOP != 0 {
		t.Errorf("signal CurrentUOP = %d, want 0 (no bins of this payload)", hit.CurrentUOP)
	}
}

// findLoopBelowThresholdSignal scans outbox rows for a LoopBelowThresholdSignal
// envelope addressed to the given station and decodes it. Mirrors
// findDemandSignal's pattern in wiring_kanban_test.go.
func findLoopBelowThresholdSignal(t *testing.T, msgs []*messaging.OutboxMessage, stationID string) *protocol.LoopBelowThresholdSignal {
	t.Helper()
	wantType := "data." + protocol.SubjectLoopBelowThreshold
	for _, m := range msgs {
		if m.MsgType != wantType || m.StationID != stationID {
			continue
		}
		var env protocol.Envelope
		testutil.MustNoErr(t, json.Unmarshal(m.Payload, &env), "decode envelope")
		var data protocol.Data
		testutil.MustNoErr(t, json.Unmarshal(env.Payload, &data), "decode data wrapper")
		var sig protocol.LoopBelowThresholdSignal
		testutil.MustNoErr(t, json.Unmarshal(data.Body, &sig), "decode LoopBelowThresholdSignal body")
		return &sig
	}
	return nil
}

// countLoopBelowThresholdSignals counts outbox rows that are
// LoopBelowThresholdSignal envelopes addressed to the given station.
func countLoopBelowThresholdSignals(msgs []*messaging.OutboxMessage, stationID string) int {
	wantType := "data." + protocol.SubjectLoopBelowThreshold
	n := 0
	for _, m := range msgs {
		if m.MsgType == wantType && m.StationID == stationID {
			n++
		}
	}
	return n
}

// outbox helper reused from wiring_kanban_test.go — both files are in
// the same package so we don't need to redefine it.
var _ = outboxSummary
