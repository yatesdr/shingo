package www

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"shingoedge/domain"
	"shingoedge/store/processes"
)

// handlers_styles_leg_publish_test.go — editing a claim's LEG re-publishes the
// process to Core.
//
// The legs (inbound_source, outbound_destination, paired_core_node,
// second_paired_core_node) are the arcs of the material loop through a cell,
// and Core's loop compiler reads them out of the plant-claims mirror. The
// mirror is only as fresh as the last publish, so an edit that changed a leg
// and did not publish would leave Core compiling the loop the plant used to
// have — for up to an hour, until the safety snapshot, with nothing on any
// screen saying so.
//
// The publish is unconditional today: apiUpsertStyleNodeClaim calls
// publishSpecChangeForStyle after every successful UpsertClaim and compares no
// fields, so it cannot miss one. This test is what would catch a field-diff
// "optimisation" being added in front of it.

// legPublishRecorder collects the process ids the spec-change hook is called
// with. Unlike specChangeRecorder in router_spec_change_test.go it does not
// block in the hook — these tests are about WHETHER an edit publishes, not
// about what the coalescer does while it is busy.
type legPublishRecorder struct {
	mu    sync.Mutex
	calls []int64
}

func (r *legPublishRecorder) record(processID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, processID)
}

func (r *legPublishRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = nil
}

// awaitPublish waits for the coalescer's window plus slack and returns what
// the hook was called with.
func (r *legPublishRecorder) awaitPublish(t *testing.T, window time.Duration) []int64 {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		n := len(r.calls)
		r.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(window / 5)
	}
	time.Sleep(window)
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int64(nil), r.calls...)
}

// startSpecChangeCoalescer wires the doorbell, the loop and a recording hook
// onto a Handlers built by newAdminRouter, the way main.go wires the real
// publisher onto the one NewRouter builds.
func startSpecChangeCoalescer(t *testing.T, h *Handlers, window time.Duration) *legPublishRecorder {
	t.Helper()
	rec := &legPublishRecorder{}
	h.specChangeCh = make(chan struct{}, 1)
	h.specChangeStop = make(chan struct{})
	h.specChangeWindow = window
	h.SetPlantSpecChangeHook(
		func(processID int64) { rec.record(processID) },
		func() { rec.record(specChangeAllMarker) },
	)
	go h.specChangeLoop()
	t.Cleanup(func() { close(h.specChangeStop) })
	return rec
}

func TestStyleNodeClaim_EditingAnyLegRepublishesTheProcess(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)
	const window = 40 * time.Millisecond
	rec := startSpecChangeCoalescer(t, h, window)

	pid := seedProcess(t, "LegPublishLine")
	sid := seedStyle(t, "LegPublishStyle", pid)

	// The press-index back-node auto-provisioning logs "front node ... not
	// found in process" on every save here. That is the handler doing what it
	// documents — it logs and carries on, because provisioning is a
	// convenience for the fleet manager's coordinates and not part of saving a
	// claim. Seeding process_nodes to silence it would add a fixture that has
	// nothing to do with whether an edit publishes.

	// The starting claim, with every leg already set, so each case below
	// changes exactly ONE field and nothing else. two_robot_press_index
	// because it is the only mode whose validation ACCEPTS all four at once:
	// single_robot refuses both paired positions outright ("does not use
	// Paired Core Node; clear it"), so a fixture on that mode could not carry
	// the thing this test is about.
	base := processes.NodeClaimInput{
		StyleID:              sid,
		CoreNodeName:         "LEG-NODE-1",
		Role:                 "consume",
		SwapMode:             "two_robot_press_index",
		PayloadCode:          "SYN-PART-A",
		ReorderPoint:         10,
		InboundSource:        "SYN-SMN-SOURCE",
		OutboundDestination:  "SYN-SMN-DEST",
		PairedCoreNode:       "LEG-NODE-2",
		SecondPairedCoreNode: "LEG-NODE-3",
		AutoReorder:          domain.Ptr(true),
	}
	resp := doRequest(t, router, "POST", "/api/style-node-claims", base, cookie)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("seed claim refused: %d %v", resp.StatusCode, decodeBody(t, resp))
	}
	if got := rec.awaitPublish(t, window); len(got) != 1 || got[0] != pid {
		t.Fatalf("creating the claim published %v, want [%d] — the publish is what makes "+
			"Core's mirror match the spec at all", got, pid)
	}

	for _, tc := range []struct {
		leg   string
		value string
		apply func(*processes.NodeClaimInput, string)
		read  func(processes.NodeClaim) string
	}{
		{"inbound_source", "SYN-SMN-SOURCE-2",
			func(in *processes.NodeClaimInput, v string) { in.InboundSource = v },
			func(c processes.NodeClaim) string { return c.InboundSource }},
		{"outbound_destination", "SYN-SMN-DEST-2",
			func(in *processes.NodeClaimInput, v string) { in.OutboundDestination = v },
			func(c processes.NodeClaim) string { return c.OutboundDestination }},
		{"paired_core_node", "LEG-NODE-4",
			func(in *processes.NodeClaimInput, v string) { in.PairedCoreNode = v },
			func(c processes.NodeClaim) string { return c.PairedCoreNode }},
		{"second_paired_core_node", "LEG-NODE-5",
			func(in *processes.NodeClaimInput, v string) { in.SecondPairedCoreNode = v },
			func(c processes.NodeClaim) string { return c.SecondPairedCoreNode }},
	} {
		t.Run(tc.leg, func(t *testing.T) {
			rec.reset()
			body := base
			tc.apply(&body, tc.value)
			resp := doRequest(t, router, "POST", "/api/style-node-claims", body, cookie)
			assertStatus(t, resp, http.StatusOK)

			// The edit landed on the row...
			claims, err := testDB.ListStyleNodeClaims(sid)
			if err != nil {
				t.Fatalf("list claims: %v", err)
			}
			if len(claims) != 1 {
				t.Fatalf("claims = %d, want 1", len(claims))
			}
			if got := tc.read(claims[0]); got != tc.value {
				t.Fatalf("stored %s = %q, want %q — the edit did not land, so what follows "+
					"would be testing a publish of nothing", tc.leg, got, tc.value)
			}

			// ...and the process was re-published because of it.
			if got := rec.awaitPublish(t, window); len(got) != 1 || got[0] != pid {
				t.Errorf("changing %s published %v, want [%d].\n"+
					"Every leg edit has to reach Core: the mirror is only as fresh as the last "+
					"publish, and a stale leg makes the loop compiler describe the plant the cell "+
					"used to be — silently, until the hourly safety snapshot.", tc.leg, got, pid)
			}
			base = body
		})
	}
}
