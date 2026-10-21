//go:build docker

package dispatch

import (
	"fmt"
	"testing"

	"shingo/protocol/testutil"
	"shingo/shared/loadervectors"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
)

// loader_side_cycle_parts_docker_test.go — a release's full-in that names
// several parts, decided in one ReplenishLoader call under the loader's lock.

// TestConsumeVectors_CoreOneAsk runs the consume cases whose releases come one
// after another, each part once, as ONE side-cycle ask naming the parts in the
// releases' order: the fulls ordered are the vectors', the same as one ask per
// release (TestConsumeVectors_Core). A burst case or one that offers a part
// twice is a set of separate asks by construction and is skipped here.
func TestConsumeVectors_CoreOneAsk(t *testing.T) {
	t.Parallel()
	ran := 0
	for i, c := range loadervectors.MustLoad().Consume {
		seen := map[string]bool{}
		distinct := true
		for _, p := range c.Events {
			distinct = distinct && !seen[p]
			seen[p] = true
		}
		if c.Burst || !distinct || len(c.Events) == 0 {
			continue
		}
		ran++
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			d, _ := newTestDispatcher(t, db, nil)
			req, cfg, back := consumeFixture(t, db, fmt.Sprintf("CO%d", i), c)
			res, err := sideCycleParts(d, req, cfg, c.Events...)
			testutil.MustNoErr(t, err, "one ask for every release's part")
			if len(res.Parts) != len(c.Events) {
				t.Fatalf("parts answered = %d, want %d (one per part named)", len(res.Parts), len(c.Events))
			}
			got := map[string][]string{}
			for _, o := range res.Created {
				got[back[o.DeliveryNode]] = append(got[back[o.DeliveryNode]], o.PayloadCode)
			}
			want := map[string][]string{}
			for w, p := range c.WantNew {
				want[w] = []string{p}
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("new fulls by window = %v, want %v\nwhy this case exists: %s", got, want, c.Why)
			}
		})
	}
	if ran < 3 {
		t.Errorf("only %d consume cases ran as one ask; the set should give several", ran)
	}
}

// TestSideCycleParts_EachPartHasItsOutcome: one ask naming three parts at a
// two-window unloader whose first window holds a full of the first part.
// The first is stopped for the full standing (Skipped), the second is placed
// on the free window, and the third is held: no window is left, and the
// window the second took is named.
func TestSideCycleParts_EachPartHasItsOutcome(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, nil)
	c := loadervectors.ConsumeCase{Payloads: []string{"PART-A", "PART-B", "PART-C"},
		Windows: []loadervectors.ConsumeWindow{{Node: "W1", StandingFull: "PART-A"}, {Node: "W2"}}}
	req, cfg, back := consumeFixture(t, db, "SPO", c)
	res, err := sideCycleParts(d, req, cfg, "PART-A", "PART-B", "PART-C")
	testutil.MustNoErr(t, err, "the ask")
	if len(res.Parts) != 3 {
		t.Fatalf("parts = %+v, want three", res.Parts)
	}
	a, b, cc := res.Parts[0], res.Parts[1], res.Parts[2]
	if a.PayloadCode != "PART-A" || a.Skipped == "" || len(a.Created) != 0 {
		t.Errorf("PART-A = %+v, want skipped for the full standing", a)
	}
	if b.PayloadCode != "PART-B" || len(b.Created) != 1 || back[b.Created[0].DeliveryNode] != "W2" {
		t.Errorf("PART-B = %+v, want one full at W2", b)
	}
	if cc.PayloadCode != "PART-C" || len(cc.Created) != 0 || cc.Skipped != "" || len(cc.HeldBy) != 2 {
		t.Errorf("PART-C = %+v, want held at both windows", cc)
	}
	if len(res.Created) != 1 {
		t.Errorf("created = %d, want 1 across the parts", len(res.Created))
	}
}

// TestSideCycleParts_TwentyPartsAtOneWindowIsOneAsk counts what Core spends on
// a twenty-payload one-window unloader's pickup, through the door's two calls
// (the loader's config, then the decision): twenty asks of one part each, as
// the Edge sent before, against one ask naming the twenty. After the first
// part takes the only window, the other nineteen read nothing: the one ask
// costs exactly what an ask for one part does.
func TestSideCycleParts_TwentyPartsAtOneWindowIsOneAsk(t *testing.T) {
	t.Parallel()
	parts := make([]string, 20)
	for i := range parts {
		parts[i] = fmt.Sprintf("PART-%02d", i)
	}
	cost := func(asks [][]string) (int64, int) {
		db, cfgDB := testdb.OpenWithConfig(t)
		setupTestData(t, db)
		market := &nodes.Node{Name: "TPW-FG", Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(market), "FG market")
		id, err := db.CreateLoader(loaders.Loader{Name: "TPW", Role: loaders.RoleConsume, Layout: loaders.LayoutSharedWindow,
			Replenishment: loaders.ReplenishmentOperator, InboundSource: market.Name})
		testutil.MustNoErr(t, err, "unloader")
		w := &nodes.Node{Name: "TPW-W1", Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(w), "window")
		testutil.MustNoErr(t, db.UpsertLoaderHome(loaders.Home{LoaderID: id, PositionNodeID: w.ID}), "home")
		for _, p := range parts {
			testutil.MustNoErr(t, db.UpsertLoaderPayload(loaders.Payload{LoaderID: id, PayloadCode: p}), "payload "+p)
		}
		cdb, counter, err := store.OpenCounting(cfgDB)
		testutil.MustNoErr(t, err, "counting db")
		t.Cleanup(func() { cdb.Close() })
		d, _ := newTestDispatcher(t, cdb, nil)
		req := ReplenishRequest{StationID: "edge.line1", LoaderID: id}
		counter.Reset()
		made := 0
		for _, a := range asks {
			cfg, ok, err := d.LoadReplenishConfig(id)
			if err != nil || !ok {
				t.Fatalf("config: ok=%v err=%v", ok, err)
			}
			res, err := sideCycleParts(d, req, cfg, a...)
			testutil.MustNoErr(t, err, "ask")
			made += len(res.Created)
		}
		return counter.Count(), made
	}
	singly := make([][]string, len(parts))
	for i, p := range parts {
		singly[i] = []string{p}
	}
	before, madeBefore := cost(singly)
	after, madeAfter := cost([][]string{parts})
	one, _ := cost([][]string{parts[:1]})
	t.Logf("twenty parts at one window: twenty asks %d statements, one ask %d, one part alone %d", before, after, one)
	if madeBefore != 1 || madeAfter != 1 {
		t.Fatalf("fulls made: %d and %d, want 1 each (one window)", madeBefore, madeAfter)
	}
	// One ask is one config read and the first part's whole decision; the
	// other nineteen parts read nothing. Twenty asks are twenty config reads
	// and, after the first, a subtraction and a window choice each.
	if after != one || after >= before {
		t.Errorf("one ask for twenty parts cost %d statements, one part alone %d, twenty asks %d; want the first two equal and under the third", after, one, before)
	}
}
