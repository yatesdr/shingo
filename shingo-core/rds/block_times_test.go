package rds

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// startTime / terminateTime have been on the /orderDetails wire the whole
// time — verified against live Springfield RDS on 2026-07-25 — and
// BlockDetail simply had no fields to hold them, which is why leg
// decomposition was recorded as blocked on a vendor limitation for two years.
// These pin the parse and the hand-off.

func TestBlockDetail_ParsesVendorTimes(t *testing.T) {
	t.Parallel()
	// Shape copied from the live probe: epoch SECONDS, on the FINISHED block.
	raw := `{"blockId":"PP3abc86a5","location":"PP66","state":"FINISHED","binTask":"Load",
	         "startTime":1784956669,"terminateTime":1784956774}`

	var b BlockDetail
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if b.StartTime != 1784956669 || b.TerminateTime != 1784956774 {
		t.Fatalf("times = %d/%d, want 1784956669/1784956774", b.StartTime, b.TerminateTime)
	}
	if got := b.TerminateTime - b.StartTime; got != 105 {
		t.Fatalf("leg duration = %ds, want 105", got)
	}
}

// A vendor that omits the fields leaves them 0 — which must read as "not
// reported", never as "instant". The probe only sampled a SINGLE-block order,
// so whether every block of a multi-block order carries times is unverified;
// this is the behaviour when they don't.
func TestBlockDetail_MissingTimesAreZero(t *testing.T) {
	t.Parallel()
	var b BlockDetail
	if err := json.Unmarshal([]byte(`{"blockId":"b1","state":"FINISHED"}`), &b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if b.StartTime != 0 || b.TerminateTime != 0 {
		t.Fatalf("absent times should be 0, got %d/%d", b.StartTime, b.TerminateTime)
	}
}

// The poller must carry the times through to the emitter. Without this the
// fields are parsed and dropped one layer later, which is the same bug in a
// different place.
func TestPollerCarriesBlockTimesToEmitter(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := hits.Add(1)
		blockState := "FINISHED"
		if n == 1 {
			blockState = "RUNNING" // baseline sample
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","id":"rds-times","state":"RUNNING","vehicle":"AMR-02","blocks":[
			{"blockId":"b-pickup","location":"AP-SOURCE","state":"` + blockState + `","binTask":"Load",
			 "startTime":1784956669,"terminateTime":1784956774}
		]}`))
	}))
	defer srv.Close()

	emitter := &mockPollerEmitter{}
	p := NewPoller(NewClient(srv.URL, 2*time.Second), emitter, &mockResolver{}, time.Minute)
	p.Track("rds-times")

	p.poll() // baseline
	p.poll() // transition into FINISHED

	emitter.mu.Lock()
	defer emitter.mu.Unlock()
	if len(emitter.blockEvents) != 1 {
		t.Fatalf("want exactly 1 block event, got %d", len(emitter.blockEvents))
	}
	got := emitter.blockEvents[0]
	if got.startTime != 1784956669 || got.terminateTime != 1784956774 {
		t.Fatalf("emitter got times %d/%d, want 1784956669/1784956774 — parsed but dropped",
			got.startTime, got.terminateTime)
	}
}
