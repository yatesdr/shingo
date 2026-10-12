//go:build docker

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/config"
	"shingocore/dispatch"
	"shingocore/dispatch/binresolver"
	"shingocore/engine"
	"shingocore/internal/testdb"
	"shingocore/plantspec"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/plantclaims"
	"shingocore/store/reservations"
)

// planner_golden_docker_test.go — the planner golden: need → chosen bin,
// destination, verdict, over today's deciders, on the demo plant.
//
// ── WHAT IT IS FOR ────────────────────────────────────────────────────────
//
// The planner phase folds today's deciders into one pure package with the
// callers unchanged. "Unchanged" is only checkable if what each decider answers
// is written down first, for every door and every shape that reaches it. This
// is that record. Each case is one need put to one decider (or one door, where
// the decider is private to it) on a freshly seeded plant, and its answer is
// stored as JSON under testdata/planner-golden. A refactor that keeps the
// callers unchanged keeps every file byte-identical.
//
// ── WHY IT LIVES IN THE SEEDER ────────────────────────────────────────────
//
// The plant is plants/demo.yaml, seeded by seedCore: the production seeder,
// which is in this package and nowhere importable. A hand-built plant would pin
// the deciders against a shape the sim never runs; the demo plant is the one
// every sim pair runs, and it has what the golden needs to be wide — a shared
// multi-payload market of depth-3 lanes, empties in their own lanes, a
// maintained group, loaders and an unloader, stations of every kind.
//
// ── WHY THE ENGINE IS STARTED AND STOPPED ─────────────────────────────────
//
// The dispatcher and the scanner are built in Engine.Start, and Start also
// launches the level keeper, the threshold monitor, the sourceability monitor
// and the sweeps, any of which can write orders on their own. So each case
// starts the engine, stops it at once, and waits until no background write
// lands; from then on every decision is made by a synchronous call from this
// test. A case asserts the plant is still quiet before it runs.
//
// ── WHAT IS RECORDED, AND WHAT IS NOT ─────────────────────────────────────
//
// Names, never ids: bins by label, nodes by name, orders by their position in
// the case. Codes, never sentences: an outcome, a queue code and cause, a term
// code — wording changes are not planner changes. No vendor ids (random) and no
// timestamps.
//
// Regenerate with -update-golden, and only for a change whose diff is labelled
// a ruling or a flat bug in the phase's prediction file.

var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/planner-golden from today's deciders")

const goldenDir = "testdata/planner-golden"

// goldenPlant is one case's world: the demo plant seeded into a fresh database,
// and an engine whose background loops have been stopped.
type goldenPlant struct {
	t  *testing.T
	db *store.DB
	// cdb is the same database opened through store.OpenCounting: the engine,
	// the picker and the resolver read through it, so every statement they
	// issue is counted and the planner's can be judged (the zero-write test,
	// planner_zero_write_docker_test.go). Fixtures and the record are written
	// and read through db, and are not counted.
	cdb     *store.DB
	counter *store.QueryCounter
	zw      *zeroWrite
	eng     *engine.Engine
	backend *testdb.MockTrackingBackend

	// What a door said that leaves no order row behind.
	refusals []string
	events   []string
	watching bool
}

func newGoldenPlant(t *testing.T) *goldenPlant {
	t.Helper()
	db, dbcfg := testdb.OpenWithConfig(t)
	plant, err := plantspec.Load("../../../plants/demo.yaml")
	testutil.MustNoErr(t, err, "load plants/demo.yaml")
	testutil.MustNoErr(t, plant.Validate(), "validate plants/demo.yaml")
	testutil.MustNoErr(t, seedCore(db, plant, map[string]int64{}), "seed the demo plant")
	seedClaimsMirror(t, db, plant)

	// ── FIFO AGE, MADE DETERMINISTIC ──────────────────────────────────────
	//
	// The finders order by COALESCE(loaded_at, created_at) and nothing else
	// (bins.FindSourceFIFO), and the seeder stamps loaded_at to the SECOND. So
	// bins seeded in one second tie, a seed that straddles a second boundary
	// splits them differently from one that does not, and LIMIT 1 over a tie is
	// whatever the plan returns. Measured: "SHIM anywhere" answered two
	// different bins across runs.
	//
	// The fixture keeps the ages the seeder MEANT (BIN-PA-03 is an hour older
	// than its lane on purpose, so FIFO digs for it) by rounding each bin's age
	// to the minute relative to the newest, and breaks every remaining tie by
	// seed order, one millisecond apart. The finders' own tie-break (b.id,
	// added in B1) agrees with that spacing, so neither decides for the other.
	_, err = db.DB.Exec(`
		WITH newest AS (SELECT MAX(loaded_at) AS t FROM bins)
		UPDATE bins SET loaded_at = TIMESTAMPTZ '2026-01-01 12:00:00+00'
		       + ROUND(EXTRACT(EPOCH FROM (bins.loaded_at - newest.t)) / 60) * INTERVAL '1 minute'
		       + bins.id * INTERVAL '1 millisecond'
		FROM newest WHERE bins.loaded_at IS NOT NULL`)
	testutil.MustNoErr(t, err, "normalise FIFO age")

	backend := testdb.NewTrackingBackend()
	cfg := config.Defaults()
	cfg.Messaging.StationID = "test-core"
	cfg.Messaging.DispatchTopic = "shingo.dispatch"
	cdb, counter, err := store.OpenCounting(dbcfg)
	testutil.MustNoErr(t, err, "open the counting store")
	t.Cleanup(func() { cdb.Close() })
	eng := engine.New(engine.Config{AppConfig: cfg, DB: cdb, Fleet: backend, LogFunc: func(string, ...any) {}})
	eng.Start()
	eng.Stop()
	g := &goldenPlant{t: t, db: db, cdb: cdb, counter: counter, eng: eng, backend: backend}
	g.settle()
	if n := g.count(`SELECT COUNT(*) FROM orders`); n != 0 {
		t.Fatalf("fixture: %d order(s) exist after the engine was started and stopped — a background "+
			"loop wrote before it was stopped, so the cases below would not be deterministic", n)
	}
	counter.Reset()
	g.watchPlanner()
	return g
}

// seedClaimsMirror fills Core's plant-claims mirror (style_claims) from the
// spec's claims, through the store door the plant.claims handler writes with.
//
// A FIXTURE CORRECTION (B1). The golden first pinned a plant whose mirror was
// empty, which no synced plant has: the Edge publishes its claims on connect.
// The empties readers exclude a carrier standing on a claimed cell position
// (bins.EmptyCarrierWhere's style_claims arm), so with the mirror empty the level
// keeper took the carriers on the presses' own positions, a behaviour only a
// wiped or late-joining Core shows. Process and style are keyed by their spec
// names; the empties arm reads only core_node_name.
func seedClaimsMirror(t *testing.T, db *store.DB, plant *plantspec.Plant) {
	t.Helper()
	processOf := map[string]string{}
	for _, st := range plant.Styles {
		processOf[st.Name] = st.Process
	}
	active := map[string]string{}
	for _, pr := range plant.Processes {
		active[pr.Name] = pr.ActiveStyle
	}
	styles := map[string][]plantclaims.StyleRow{}
	claims := map[string][]plantclaims.ClaimRow{}
	seen := map[string]bool{}
	var order []string
	for i, c := range plant.Claims {
		proc := processOf[c.Style]
		if proc == "" {
			t.Fatalf("fixture: claim on %s names style %q, which no process owns", c.CoreNode, c.Style)
		}
		if !seen[proc] {
			seen[proc] = true
			order = append(order, proc)
		}
		if key := proc + "|" + c.Style; !seen[key] {
			seen[key] = true
			styles[proc] = append(styles[proc], plantclaims.StyleRow{ProcessID: proc, StyleID: c.Style, IsActive: active[proc] == c.Style})
		}
		claims[proc] = append(claims[proc], plantclaims.ClaimRow{
			ProcessID: proc, StyleID: c.Style, CoreNodeName: c.CoreNode,
			Role: protocol.ClaimRole(c.Role), SwapMode: protocol.SwapMode(c.SwapMode),
			PayloadCode: c.Payload, UOPCapacity: int(c.UOPCapacity), ReorderPoint: int(c.ReorderPoint), Seq: i,
			InboundSource: c.InboundSource, OutboundDestination: c.OutboundDestination, PairedCoreNode: c.PairedCoreNode,
		})
	}
	for _, proc := range order {
		testutil.MustNoErr(t, db.ReplacePlantClaims(proc, styles[proc], claims[proc], 0), "seed claims mirror for "+proc)
	}
}

// settle drains the scan Start launched and waits for the order count to hold.
func (g *goldenPlant) settle() {
	g.eng.RunFulfillmentScan()
	last := -1
	for i := 0; i < 20; i++ {
		n := g.count(`SELECT COUNT(*) FROM orders`)
		if n == last {
			return
		}
		last = n
		time.Sleep(25 * time.Millisecond)
	}
}

func (g *goldenPlant) count(q string, args ...any) int {
	g.t.Helper()
	var n int
	testutil.MustNoErr(g.t, g.db.DB.QueryRow(q, args...).Scan(&n), "count: "+q)
	return n
}

func (g *goldenPlant) node(name string) *nodes.Node {
	g.t.Helper()
	n, err := g.db.GetNodeByName(name)
	testutil.MustNoErr(g.t, err, "node "+name)
	return n
}

func (g *goldenPlant) bin(label string) *bins.Bin {
	g.t.Helper()
	var id int64
	testutil.MustNoErr(g.t, g.db.DB.QueryRow(`SELECT id FROM bins WHERE label = $1`, label).Scan(&id), "bin "+label)
	b, err := g.db.GetBin(id)
	testutil.MustNoErr(g.t, err, "bin "+label)
	return b
}

func (g *goldenPlant) exec(q string, args ...any) {
	g.t.Helper()
	_, err := g.db.DB.Exec(q, args...)
	testutil.MustNoErr(g.t, err, q)
}

// holder makes an order that exists only to hold something, so a case can take
// a bin or a slot away from the need under test. It is `dispatched` with a
// vendor id: an order that is on its way, which is the only legal holder of a
// hard claim.
func (g *goldenPlant) holder(uuid string) *orders.Order {
	g.t.Helper()
	return testdb.CreateOrder(g.t, g.db, func(o *orders.Order) {
		o.EdgeUUID = uuid
		o.OrderType = protocol.OrderTypeMove
		o.Status = protocol.StatusDispatched
		o.VendorOrderID = "vendor-" + uuid
	})
}

// claimBin takes a bin away the way dispatch does: a reservation, then the claim.
func (g *goldenPlant) claimBin(label string, by *orders.Order) {
	g.t.Helper()
	testdb.ClaimBinForTest(g.t, g.db, g.bin(label).ID, by.ID)
}

func (g *goldenPlant) labelOf(binID *int64) string {
	if binID == nil {
		return ""
	}
	var label string
	if err := g.db.DB.QueryRow(`SELECT label FROM bins WHERE id = $1`, *binID).Scan(&label); err != nil {
		return fmt.Sprintf("<unknown bin: %v>", err)
	}
	return label
}

func (g *goldenPlant) finder() *dispatch.SourceFinder {
	return dispatch.NewSourceFinder(g.cdb, &dispatch.DefaultResolver{DB: g.cdb}, nil)
}

// ── the recorded shapes ──────────────────────────────────────────────────────

type sourceAnswer struct {
	Outcome    string `json:"outcome"`
	Bin        string `json:"bin,omitempty"`
	Node       string `json:"node,omitempty"`
	QueueCode  string `json:"queue_code,omitempty"`
	QueueCause string `json:"queue_cause,omitempty"`
	BuriedLane string `json:"buried_lane,omitempty"`
	TermCode   string `json:"term_code,omitempty"`
	Failed     bool   `json:"failed,omitempty"`
}

func outcomeName(o dispatch.Outcome) string {
	switch o {
	case dispatch.OutcomeFound:
		return "found"
	case dispatch.OutcomeWait:
		return "wait"
	case dispatch.OutcomeReshuffle:
		return "reshuffle"
	case dispatch.OutcomeStructural:
		return "structural"
	}
	return fmt.Sprintf("outcome(%d)", int(o))
}

func (g *goldenPlant) sourceAnswer(r dispatch.SourceResult) sourceAnswer {
	a := sourceAnswer{
		Outcome:    outcomeName(r.Outcome),
		QueueCode:  string(r.QueueCode),
		QueueCause: string(r.QueueCause),
		TermCode:   r.TermCode,
		Failed:     r.Err != nil && r.Outcome == dispatch.OutcomeFound,
	}
	if r.Bin != nil {
		a.Bin = r.Bin.Label
	}
	if r.Node != nil {
		a.Node = r.Node.Name
	}
	if r.Buried != nil && r.Buried.LaneID != 0 {
		var lane string
		_ = g.db.DB.QueryRow(`SELECT name FROM nodes WHERE id = $1`, r.Buried.LaneID).Scan(&lane)
		a.BuriedLane = lane
	}
	return a
}

type resolveAnswer struct {
	Node  string `json:"node,omitempty"`
	Bin   string `json:"bin,omitempty"`
	Error bool   `json:"error,omitempty"`
}

type orderAnswer struct {
	Index        int      `json:"index"`
	Parent       int      `json:"parent,omitempty"` // index of the parent in this list, 0 = none
	Type         string   `json:"type"`
	Status       string   `json:"status"`
	Payload      string   `json:"payload,omitempty"`
	Source       string   `json:"source,omitempty"`
	Delivery     string   `json:"delivery,omitempty"`
	Bin          string   `json:"bin,omitempty"`
	QueueCode    string   `json:"queue_code,omitempty"`
	QueueCause   string   `json:"queue_cause,omitempty"`
	Steps        []string `json:"steps,omitempty"`
	HardBins     []string `json:"hard_bins,omitempty"`
	HardSlots    []string `json:"hard_slots,omitempty"`
	Reservations []string `json:"reservations,omitempty"`
	ToFleet      bool     `json:"to_fleet,omitempty"`
}

type fleetAnswer struct {
	External string   `json:"external"`
	Blocks   []string `json:"blocks"`
}

// orders renders every order in the database, in creation order, with what it
// holds. Holder orders a case made for itself are left out by uuid prefix.
func (g *goldenPlant) orders() []orderAnswer {
	g.t.Helper()
	rows, err := g.db.DB.Query(`
		SELECT id, parent_order_id, order_type, status, COALESCE(payload_code,''), COALESCE(source_node,''),
		       COALESCE(delivery_node,''), bin_id, COALESCE(queue_code,''), COALESCE(queue_cause,''),
		       COALESCE(steps_json,''), COALESCE(vendor_order_id,'')
		FROM orders WHERE edge_uuid NOT LIKE 'golden-holder-%' ORDER BY id`)
	testutil.MustNoErr(g.t, err, "list orders")
	type raw struct {
		id     int64
		parent *int64
		a      orderAnswer
		steps  string
	}
	var all []raw
	for rows.Next() {
		var r raw
		var binID *int64
		var vendor string
		testutil.MustNoErr(g.t, rows.Scan(&r.id, &r.parent, &r.a.Type, &r.a.Status, &r.a.Payload, &r.a.Source,
			&r.a.Delivery, &binID, &r.a.QueueCode, &r.a.QueueCause, &r.steps, &vendor), "scan order")
		r.a.Bin = g.labelOf(binID)
		r.a.ToFleet = vendor != ""
		all = append(all, r)
	}
	testutil.MustNoErr(g.t, rows.Err(), "orders rows")
	rows.Close()

	index := map[int64]int{}
	var out []orderAnswer
	for i, r := range all {
		index[r.id] = i + 1
		r.a.Index = i + 1
		if r.parent != nil {
			r.a.Parent = index[*r.parent]
		}
		r.a.Steps = renderSteps(r.steps)
		r.a.HardBins = g.strings(`SELECT label FROM bins WHERE claimed_by = $1 ORDER BY label`, r.id)
		r.a.HardSlots = g.strings(`SELECT name FROM nodes WHERE claimed_by = $1 ORDER BY name`, r.id)
		r.a.Reservations = g.strings(`
			SELECT r.resource_kind || ':' || r.state || ':' ||
			       COALESCE((SELECT label FROM bins WHERE id = r.bin_id), (SELECT name FROM nodes WHERE id = r.node_id), '')
			FROM reservations r WHERE r.order_id = $1 AND r.state <> 'released' ORDER BY 1`, r.id)
		out = append(out, r.a)
	}
	return out
}

func (g *goldenPlant) strings(q string, args ...any) []string {
	g.t.Helper()
	rows, err := g.db.DB.Query(q, args...)
	testutil.MustNoErr(g.t, err, q)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		testutil.MustNoErr(g.t, rows.Scan(&s), "scan string")
		out = append(out, s)
	}
	return out
}

// renderSteps keeps a stored step list's decisions: the action, the node, and
// whether a pickup asks for an empty.
func renderSteps(stepsJSON string) []string {
	if stepsJSON == "" {
		return nil
	}
	var steps []map[string]any
	if err := json.Unmarshal([]byte(stepsJSON), &steps); err != nil {
		return []string{"<unparseable: " + err.Error() + ">"}
	}
	var out []string
	for _, s := range steps {
		line := fmt.Sprint(s["action"], " ", s["node"])
		if e, ok := s["empty"].(bool); ok && e {
			line += " (empty)"
		}
		if p, ok := s["payload_code"].(string); ok && p != "" {
			line += " [" + p + "]"
		}
		out = append(out, line)
	}
	return out
}

func (g *goldenPlant) fleet() []fleetAnswer {
	var out []fleetAnswer
	for _, r := range g.backend.CreateRequests() {
		f := fleetAnswer{External: g.orderRef(r.ExternalID)}
		for _, b := range r.Blocks {
			f.Blocks = append(f.Blocks, b.Location+" "+b.BinTask)
		}
		out = append(out, f)
	}
	return out
}

// orderRef names an order by the uuid the case chose, or, for a uuid Core
// minted (compound legs get random ones), by its index in orders().
func (g *goldenPlant) orderRef(uuid string) string {
	if strings.HasPrefix(uuid, "g-") {
		return uuid
	}
	var id int64
	if err := g.db.DB.QueryRow(`SELECT id FROM orders WHERE edge_uuid = $1`, uuid).Scan(&id); err != nil {
		return "<unknown order>"
	}
	return fmt.Sprintf("order %d of this case", g.count(
		`SELECT COUNT(*) FROM orders WHERE id <= $1 AND edge_uuid NOT LIKE 'golden-holder-%'`, id))
}

// refused records a door's refusal by kind: the error's type and, for a bin
// move, its refusal kind. The sentence is left out.
func (g *goldenPlant) refused(err error) {
	if err == nil {
		return
	}
	kind := fmt.Sprintf("%T", err)
	var bm *engine.BinMoveError
	if errors.As(err, &bm) {
		kind += " " + [...]string{"bad-request", "conflict", "fault"}[bm.Kind]
	}
	g.refusals = append(g.refusals, kind)
}

// watch records the order events a door emits, which is how a wire request
// refused at intake (no order row) shows its answer.
func (g *goldenPlant) watch() {
	if g.watching {
		return
	}
	g.watching = true
	g.eng.Events.Subscribe(func(evt engine.Event) {
		if name := eventName(evt.Type); name != "" {
			g.events = append(g.events, name)
		}
	})
}

func eventName(t engine.EventType) string {
	switch t {
	case engine.EventOrderReceived:
		return "received"
	case engine.EventOrderDispatched:
		return "dispatched"
	case engine.EventOrderFailed:
		return "failed"
	case engine.EventOrderCancelled:
		return "cancelled"
	}
	return ""
}

// replenish asks the loader-replenish door for one reading below threshold.
func (g *goldenPlant) replenish(loaderName, payload, member string, threshold, current int) {
	g.t.Helper()
	ls, err := g.db.ListLoaders()
	testutil.MustNoErr(g.t, err, "list loaders")
	var id int64
	for _, l := range ls {
		if l.Name == loaderName {
			id = l.ID
		}
	}
	if id == 0 {
		g.t.Fatalf("fixture: the demo plant has no loader %s", loaderName)
	}
	d := g.eng.Dispatcher()
	cfg, ok, err := d.LoadReplenishConfig(id)
	testutil.MustNoErr(g.t, err, "replenish config "+loaderName)
	if !ok {
		g.refusals = append(g.refusals, "no replenish config")
		return
	}
	pl, err := g.db.GetPayloadByCode(payload)
	testutil.MustNoErr(g.t, err, "payload "+payload)
	res, err := d.ReplenishLoader(dispatch.ReplenishRequest{
		StationID: "edge1.line1", LoaderID: id, PayloadCode: payload, MemberNode: member,
		Threshold: threshold, CurrentUOP: current, PerBinCapacity: pl.UOPCapacity,
	}, cfg)
	g.refused(err)
	g.refusals = append(g.refusals, fmt.Sprintf("want %d", res.Want))
	g.eng.RunFulfillmentScan()
}

// replies renders what Core queued for the Edge after outbox id `since`: the
// message type and, where the payload carries them, the status and error code.
// This is the only record of a wire request refused at intake.
func (g *goldenPlant) replies(since int) []string {
	g.t.Helper()
	rows, err := g.db.DB.Query(`SELECT msg_type, payload FROM outbox WHERE id > $1 ORDER BY id`, since)
	testutil.MustNoErr(g.t, err, "outbox")
	defer rows.Close()
	var out []string
	for rows.Next() {
		var typ string
		var payload []byte
		testutil.MustNoErr(g.t, rows.Scan(&typ, &payload), "scan outbox")
		// NOT A DOOR'S REPLY. The sourceability monitor pushes a station's
		// sourcing state on its own debounce when the bins it watches move, and
		// the engine's Stop does not end that push. Whether one lands inside a
		// case's window is timing, not a decision: seen once in five runs of the
		// full golden, and it made the fifo-dig case flake. The doors record
		// what they answered, so the push is left out.
		if typ == "data."+protocol.SubjectSourcingState {
			continue
		}
		var v any
		_ = json.Unmarshal(payload, &v)
		line := typ
		for _, k := range []string{"status", "error_code"} {
			if f := findKey(v, k); f != "" {
				line += " " + k + "=" + f
			}
		}
		out = append(out, line)
	}
	return out
}

// findKey returns the first string value under key anywhere in a decoded JSON
// value, depth-first in key order.
func findKey(v any, key string) string {
	switch x := v.(type) {
	case map[string]any:
		if s, ok := x[key].(string); ok {
			return s
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if f := findKey(x[k], key); f != "" {
				return f
			}
		}
	case []any:
		for _, e := range x {
			if f := findKey(e, key); f != "" {
				return f
			}
		}
	}
	return ""
}

// golden compares one case's record with its committed file.
func golden(t *testing.T, name string, need, decision any) {
	t.Helper()
	got, err := json.MarshalIndent(struct {
		Need     any `json:"need"`
		Decision any `json:"decision"`
	}{need, decision}, "", "  ")
	testutil.MustNoErr(t, err, "marshal "+name)
	got = append(got, '\n')
	path := filepath.Join(goldenDir, name+".json")
	if *updateGolden {
		testutil.MustNoErr(t, os.MkdirAll(filepath.Dir(path), 0o755), "mkdir golden")
		testutil.MustNoErr(t, os.WriteFile(path, got, 0o644), "write "+path)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no golden for %s (%v). Run with -update-golden once, read the file, and commit it.", name, err)
	}
	if !bytes.Equal(bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")), got) {
		t.Errorf("planner golden %s moved.\n--- committed\n%s\n--- today\n%s\n"+
			"A move is a stop unless the phase's PREDICTIONS.md labels it a ruling or a flat bug.",
			name, want, got)
	}
}

// ── the cases ────────────────────────────────────────────────────────────────

// TestPlannerGolden_BinPicker puts needs to the one source chooser
// (SourceFinder.FindSourceForNeed), which every door that shops for a bin
// reaches: wire intake and the scanner through FindSource, and the complex step
// resolver's four call sites directly.
func TestPlannerGolden_BinPicker(t *testing.T) {
	t.Parallel()
	requirePlannerSeen(t)
	type need struct {
		Source   string `json:"source,omitempty"`
		Payload  string `json:"payload,omitempty"`
		Delivery string `json:"delivery,omitempty"`
		Intent   string `json:"intent"`
		Local    bool   `json:"node_local,omitempty"`
		Setup    string `json:"setup,omitempty"`
		Asker    string `json:"asker,omitempty"`
	}
	cases := []struct {
		name  string
		need  need
		setup func(g *goldenPlant) reservations.DigAsker
	}{
		{name: "full-panel-a-from-market", need: need{Source: "SYN_MARKET", Payload: "PANEL-A", Delivery: "ALN_001", Intent: "full"}},
		{name: "full-panel-b-from-market", need: need{Source: "SYN_MARKET", Payload: "PANEL-B", Delivery: "ALN_003", Intent: "full"}},
		{name: "full-brkt-from-market", need: need{Source: "SYN_MARKET", Payload: "BRKT", Delivery: "ALN_004", Intent: "full"}},
		{name: "full-clip-from-market", need: need{Source: "SYN_MARKET", Payload: "CLIP", Delivery: "ALN_006", Intent: "full"}},
		{name: "full-stud-from-market", need: need{Source: "SYN_MARKET", Payload: "STUD", Delivery: "ALN_007", Intent: "full"}},
		{name: "full-none-stocked", need: need{Source: "SYN_MARKET", Payload: "ASSY", Delivery: "ALN_001", Intent: "full"}},
		{name: "full-panel-a-anywhere", need: need{Payload: "PANEL-A", Delivery: "ALN_001", Intent: "full"}},
		{name: "full-shim-anywhere", need: need{Payload: "SHIM", Delivery: "ALN_008", Intent: "full"}},
		{name: "empty-standard-from-market", need: need{Source: "SYN_MARKET", Payload: "PANEL-A", Delivery: "PLN_001", Intent: "empty"}},
		{name: "empty-standard-sm-from-market", need: need{Source: "SYN_MARKET", Payload: "PANEL-B", Delivery: "PLN_003", Intent: "empty"}},
		{name: "empty-from-press-empties", need: need{Source: "SYN_PRESS_EMPTIES", Payload: "PANEL-A", Delivery: "PLN_001", Intent: "empty"}},
		{name: "empty-anywhere", need: need{Payload: "PANEL-A", Delivery: "PLN_001", Intent: "empty"}},
		{name: "node-local-shim-at-home", need: need{Source: "PLK_H1", Payload: "SHIM", Delivery: "ALN_008", Intent: "full", Local: true}},
		{name: "node-local-at-empty-node", need: need{Source: "PLK_B1", Payload: "SHIM", Delivery: "ALN_008", Intent: "full", Local: true}},
		{
			name: "full-panel-a-front-claimed",
			need: need{Source: "SYN_MARKET", Payload: "PANEL-A", Delivery: "ALN_001", Intent: "full",
				Setup: "BIN-PA-01 (the lane front) is claimed by an order on its way"},
			setup: func(g *goldenPlant) reservations.DigAsker {
				g.claimBin("BIN-PA-01", g.holder("golden-holder-1"))
				return reservations.Anyone
			},
		},
		{
			name: "full-panel-a-all-claimed",
			need: need{Source: "SYN_MARKET", Payload: "PANEL-A", Delivery: "ALN_001", Intent: "full",
				Setup: "every PANEL-A bin is claimed"},
			setup: func(g *goldenPlant) reservations.DigAsker {
				h := g.holder("golden-holder-1")
				for _, l := range []string{"BIN-PA-01", "BIN-PA-02", "BIN-PA-03"} {
					g.claimBin(l, h)
				}
				return reservations.Anyone
			},
		},
		{
			name: "full-clip-front-locked",
			need: need{Source: "SYN_MARKET", Payload: "CLIP", Delivery: "ALN_006", Intent: "full",
				Setup: "BIN-CLIP-01 (the lane front) is locked"},
			setup: func(g *goldenPlant) reservations.DigAsker {
				g.exec(`UPDATE bins SET locked = true WHERE label = 'BIN-CLIP-01'`)
				return reservations.Anyone
			},
		},
		{
			name: "full-brkt-front-pending-reserved",
			need: need{Source: "SYN_MARKET", Payload: "BRKT", Delivery: "ALN_004", Intent: "full",
				Setup: "BIN-BRKT-01 carries another order's pending reservation"},
			setup: func(g *goldenPlant) reservations.DigAsker {
				testdb.ReserveBin(g.t, g.db, g.holder("golden-holder-1").ID, g.bin("BIN-BRKT-01").ID)
				return reservations.Anyone
			},
		},
		{
			name: "empty-standard-front-lanes-claimed",
			need: need{Source: "SYN_MARKET", Payload: "PANEL-A", Delivery: "PLN_001", Intent: "empty",
				Setup: "the front empties of Lane_11 and Lane_12 are claimed"},
			setup: func(g *goldenPlant) reservations.DigAsker {
				h := g.holder("golden-holder-1")
				g.claimBin("BIN-MT-01", h)
				g.claimBin("BIN-MT-04", h)
				return reservations.Anyone
			},
		},
		// ── OWNER-AWARE RESERVATIONS (B1, Rule 4) ─────────────────────────────
		// A pending reservation is somebody's plan for a bin. The order holding it
		// must be able to re-find it; any other order must not take it by
		// accident. Two tiers, each asked both ways: the loader pool (tier 2) and a
		// node-local move (tier 4).
		{
			name: "node-local-shim-reserved-by-stranger",
			need: need{Source: "PLK_H1", Payload: "SHIM", Delivery: "ALN_008", Intent: "full", Local: true,
				Setup: "BIN-SHIM-01 carries another order's pending reservation", Asker: "an order holding nothing"},
			setup: func(g *goldenPlant) reservations.DigAsker {
				testdb.ReserveBin(g.t, g.db, g.holder("golden-holder-1").ID, g.bin("BIN-SHIM-01").ID)
				return reservations.Anyone
			},
		},
		{
			name: "node-local-shim-reserved-by-asker",
			need: need{Source: "PLK_H1", Payload: "SHIM", Delivery: "ALN_008", Intent: "full", Local: true,
				Setup: "BIN-SHIM-01 carries the asker's own pending reservation", Asker: "the reservation's holder"},
			setup: func(g *goldenPlant) reservations.DigAsker {
				h := g.holder("golden-holder-1")
				testdb.ReserveBin(g.t, g.db, h.ID, g.bin("BIN-SHIM-01").ID)
				return reservations.DigAsker{OrderID: h.ID, LaneOwner: h.ID}
			},
		},
		{
			name: "node-local-move-reserved-by-stranger",
			need: need{Source: "PLN_001", Delivery: "SYN_MARKET", Intent: "full", Local: true,
				Setup: "BIN-ACT-P1F carries another order's pending reservation", Asker: "an order holding nothing"},
			setup: func(g *goldenPlant) reservations.DigAsker {
				testdb.ReserveBin(g.t, g.db, g.holder("golden-holder-1").ID, g.bin("BIN-ACT-P1F").ID)
				return reservations.Anyone
			},
		},
		{
			name: "node-local-move-reserved-by-asker",
			need: need{Source: "PLN_001", Delivery: "SYN_MARKET", Intent: "full", Local: true,
				Setup: "BIN-ACT-P1F carries the asker's own pending reservation", Asker: "the reservation's holder"},
			setup: func(g *goldenPlant) reservations.DigAsker {
				h := g.holder("golden-holder-1")
				testdb.ReserveBin(g.t, g.db, h.ID, g.bin("BIN-ACT-P1F").ID)
				return reservations.DigAsker{OrderID: h.ID, LaneOwner: h.ID}
			},
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			g := newGoldenPlant(t)
			asker := reservations.Anyone
			if c.setup != nil {
				asker = c.setup(g)
			}
			intent := dispatch.IntentFull
			if c.need.Intent == "empty" {
				intent = dispatch.IntentEmpty
			}
			res := g.finder().FindSourceForNeed(dispatch.SourceNeed{
				SourceNode: c.need.Source, PayloadCode: c.need.Payload, DeliveryNode: c.need.Delivery,
				Intent: intent, NodeLocal: c.need.Local, Asker: asker,
			})
			golden(t, "bin-picker/"+c.name, c.need, g.sourceAnswer(res))
		})
	}
}

// TestPlannerGolden_GroupResolver puts needs to the group resolver
// (binresolver.DefaultResolver.Resolve), the destination chooser under every
// store into a synthetic group and the retrieve-mode chooser under the finder.
func TestPlannerGolden_GroupResolver(t *testing.T) {
	t.Parallel()
	requirePlannerSeen(t)
	type need struct {
		Group   string `json:"group"`
		Mode    string `json:"mode"`
		Payload string `json:"payload,omitempty"`
		BinType string `json:"bin_type,omitempty"`
		Setup   string `json:"setup,omitempty"`
	}
	cases := []struct {
		name  string
		need  need
		setup func(g *goldenPlant)
	}{
		{name: "store-panel-a-market", need: need{Group: "SYN_MARKET", Mode: "store", Payload: "PANEL-A", BinType: "STANDARD"}},
		{name: "store-panel-b-market", need: need{Group: "SYN_MARKET", Mode: "store", Payload: "PANEL-B", BinType: "STANDARD-SM"}},
		{name: "store-clip-market", need: need{Group: "SYN_MARKET", Mode: "store", Payload: "CLIP", BinType: "STANDARD"}},
		{name: "store-assy-market", need: need{Group: "SYN_MARKET", Mode: "store", Payload: "ASSY", BinType: "STANDARD"}},
		{name: "store-empty-standard-market", need: need{Group: "SYN_MARKET", Mode: "store", BinType: "STANDARD"}},
		{name: "store-empty-standard-sm-market", need: need{Group: "SYN_MARKET", Mode: "store", BinType: "STANDARD-SM"}},
		{name: "store-empty-press-empties", need: need{Group: "SYN_PRESS_EMPTIES", Mode: "store", BinType: "STANDARD"}},
		{name: "store-clear-lane", need: need{Group: "SYN_CLEAR", Mode: "store", Payload: "PANEL-A", BinType: "STANDARD"}},
		{name: "store-unknown-bin-type", need: need{Group: "SYN_MARKET", Mode: "store", Payload: "PANEL-A"}},
		{
			name: "store-panel-a-own-lane-full",
			need: need{Group: "SYN_MARKET", Mode: "store", Payload: "PANEL-A", BinType: "STANDARD",
				Setup: "Lane_03 and Lane_07 each hold a claimed slot"},
			setup: func(g *goldenPlant) {
				h := g.holder("golden-holder-1")
				testdb.ClaimSlotForTest(g.t, g.db, g.node("SMN_009").ID, h.ID)
				testdb.ClaimSlotForTest(g.t, g.db, g.node("SMN_021").ID, h.ID)
			},
		},
		{name: "retrieve-panel-a-market", need: need{Group: "SYN_MARKET", Mode: "retrieve", Payload: "PANEL-A"}},
		{name: "retrieve-stud-market", need: need{Group: "SYN_MARKET", Mode: "retrieve", Payload: "STUD"}},
		{name: "retrieve-assy-market", need: need{Group: "SYN_MARKET", Mode: "retrieve", Payload: "ASSY"}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			g := newGoldenPlant(t)
			if c.setup != nil {
				c.setup(g)
			}
			mode := binresolver.ResolveModeStore
			if c.need.Mode == "retrieve" {
				mode = binresolver.ResolveModeRetrieve
			}
			stated := binresolver.UnknownBinType("planner golden: the need names no bin type")
			if c.need.BinType != "" {
				bt, err := g.db.GetBinTypeByCode(c.need.BinType)
				testutil.MustNoErr(t, err, "bin type "+c.need.BinType)
				stated = binresolver.KnownBinType(bt.ID)
			}
			r := &binresolver.DefaultResolver{DB: g.cdb}
			group := g.node(c.need.Group)
			var res *binresolver.ResolveResult
			var err error
			g.plannerCall("group-resolver", func() {
				res, err = r.Resolve(group, mode, c.need.Payload, stated, reservations.Anyone, nil)
			})
			var a resolveAnswer
			switch {
			case err != nil:
				a.Error = true
			case res != nil:
				if res.Node != nil {
					a.Node = res.Node.Name
				}
				if res.Bin != nil {
					a.Bin = res.Bin.Label
				}
			}
			golden(t, "group-resolver/"+c.name, c.need, a)
		})
	}
}

// TestPlannerGolden_Sourceable records the eligibility verdict for every bin in
// the plant, in both languages: the SQL predicate the finders filter with
// (bins.BinSourceableSQL, against the bin's own payload) and the Go twin the
// resolvers apply to one bin in hand (binresolver.BinUnavailableReason). The
// planner phase names them as two questions with a parity test; this is the
// answer both must keep.
//
// Some bins are perturbed first, one per disqualifier, so each arm of the
// predicate is exercised on a real row.
func TestPlannerGolden_Sourceable(t *testing.T) {
	t.Parallel()
	requirePlannerSeen(t)
	g := newGoldenPlant(t)
	h := g.holder("golden-holder-1")
	g.claimBin("BIN-PA-01", h)
	testdb.ReserveBin(t, g.db, h.ID, g.bin("BIN-PB-01").ID)
	g.exec(`UPDATE bins SET locked = true WHERE label = 'BIN-CLIP-01'`)
	g.exec(`UPDATE bins SET status = 'staged' WHERE label = 'BIN-STUD-01'`)
	g.exec(`UPDATE bins SET node_id = (SELECT id FROM nodes WHERE name = '_TRANSIT') WHERE label = 'BIN-BRKT-03'`)

	type verdict struct {
		Bin        string `json:"bin"`
		Payload    string `json:"payload,omitempty"`
		Node       string `json:"node"`
		Sourceable bool   `json:"sourceable_sql"`
		GoReason   string `json:"go_unavailable_reason,omitempty"`
	}
	var out []verdict
	rows, err := g.db.DB.Query(`
		SELECT b.id, b.label, COALESCE(b.payload_code, ''), COALESCE(n.name, ''),
		       EXISTS (SELECT 1 FROM bins b2 WHERE b2.id = b.id AND ` + bins.BinSourceableSQL("b.payload_code") + `)
		FROM bins b LEFT JOIN nodes n ON n.id = b.node_id ORDER BY b.label`)
	testutil.MustNoErr(t, err, "sourceable verdicts")
	type row struct {
		id int64
		v  verdict
	}
	var rs []row
	for rows.Next() {
		var r row
		testutil.MustNoErr(t, rows.Scan(&r.id, &r.v.Bin, &r.v.Payload, &r.v.Node, &r.v.Sourceable), "scan")
		rs = append(rs, r)
	}
	rows.Close()
	for _, r := range rs {
		b, err := g.db.GetBin(r.id)
		testutil.MustNoErr(t, err, "bin "+r.v.Bin)
		rule, err := g.db.LoadBinTypeRule(r.v.Payload)
		testutil.MustNoErr(t, err, "bin type rule "+r.v.Payload)
		g.plannerCall("pickable-at", func() { r.v.GoReason = binresolver.BinUnavailableReason(b, r.v.Payload, rule) })
		out = append(out, r.v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bin < out[j].Bin })
	golden(t, "sourceable/every-bin", map[string]any{
		"perturbed": []string{
			"BIN-PA-01 claimed by an order on its way",
			"BIN-PB-01 pending-reserved by that order",
			"BIN-CLIP-01 locked",
			"BIN-STUD-01 staged",
			"BIN-BRKT-03 in _TRANSIT",
		},
		"predicate": "SQL and the Go twin, each against the bin's own payload and that payload's bin-type rule",
	}, out)
}

// TestPlannerGolden_Doors drives each order-creation door whose deciders are
// private to it, and records what it decided: the orders it made, what each
// holds, and what went to the fleet. One door per case, on its own plant.
func TestPlannerGolden_Doors(t *testing.T) {
	t.Parallel()
	requirePlannerSeen(t)
	env := testdb.Envelope()
	wire := func(g *goldenPlant, req protocol.OrderRequest) {
		g.watch()
		g.eng.Dispatcher().HandleOrderRequest(env, &req)
		g.eng.RunFulfillmentScan()
	}
	complexReq := func(g *goldenPlant, req protocol.ComplexOrderRequest) {
		g.watch()
		g.eng.Dispatcher().HandleComplexOrderRequest(env, &req)
		g.eng.RunFulfillmentScan()
	}
	cases := []struct {
		name string
		need map[string]any
		run  func(g *goldenPlant)
	}{
		{
			name: "wire-retrieve-line",
			need: map[string]any{"door": "edge wire intake", "type": "retrieve", "payload": "STUD", "source": "SYN_MARKET", "delivery": "SLN_001"},
			run: func(g *goldenPlant) {
				wire(g, protocol.OrderRequest{OrderUUID: "g-retrieve", OrderType: protocol.OrderTypeRetrieve,
					PayloadCode: "STUD", SourceNode: "SYN_MARKET", DeliveryNode: "SLN_001", Quantity: 1})
			},
		},
		{
			name: "wire-retrieve-fifo-dig",
			need: map[string]any{"door": "edge wire intake", "type": "retrieve", "payload": "PANEL-A", "source": "SYN_MARKET", "delivery": "SLN_002",
				"note": "the oldest PANEL-A is the deepest in Lane_01, so strict FIFO plans a dig"},
			run: func(g *goldenPlant) {
				wire(g, protocol.OrderRequest{OrderUUID: "g-fifo-dig", OrderType: protocol.OrderTypeRetrieve,
					PayloadCode: "PANEL-A", SourceNode: "SYN_MARKET", DeliveryNode: "SLN_002", Quantity: 1})
			},
		},
		{
			name: "wire-retrieve-occupied-line",
			need: map[string]any{"door": "edge wire intake", "type": "retrieve", "payload": "STUD", "source": "SYN_MARKET", "delivery": "ALN_007",
				"note": "the delivery node already holds a bin"},
			run: func(g *goldenPlant) {
				wire(g, protocol.OrderRequest{OrderUUID: "g-occupied", OrderType: protocol.OrderTypeRetrieve,
					PayloadCode: "STUD", SourceNode: "SYN_MARKET", DeliveryNode: "ALN_007", Quantity: 1})
			},
		},
		{
			name: "wire-retrieve-empty-press",
			need: map[string]any{"door": "edge wire intake", "type": "retrieve_empty", "payload": "PANEL-A", "source": "SYN_PRESS_EMPTIES", "delivery": "PLN_005"},
			run: func(g *goldenPlant) {
				wire(g, protocol.OrderRequest{OrderUUID: "g-retrieve-empty", OrderType: protocol.OrderTypeRetrieveEmpty,
					PayloadCode: "PANEL-A", SourceNode: "SYN_PRESS_EMPTIES", DeliveryNode: "PLN_005", Quantity: 1})
			},
		},
		{
			name: "wire-store-type-refused",
			need: map[string]any{"door": "edge wire intake", "type": "store", "payload": "BRKT", "source": "ALN_004", "delivery": "SYN_MARKET",
				"note": "store is not a wire order type; the Edge sends a move"},
			run: func(g *goldenPlant) {
				wire(g, protocol.OrderRequest{OrderUUID: "g-store", OrderType: protocol.OrderTypeStore,
					PayloadCode: "BRKT", SourceNode: "ALN_004", DeliveryNode: "SYN_MARKET", Quantity: 1})
			},
		},
		{
			name: "wire-move-empty-to-press-empties",
			need: map[string]any{"door": "edge wire intake", "type": "move", "source": "PLN_001", "delivery": "SYN_PRESS_EMPTIES"},
			run: func(g *goldenPlant) {
				wire(g, protocol.OrderRequest{OrderUUID: "g-move", OrderType: protocol.OrderTypeMove,
					SourceNode: "PLN_001", DeliveryNode: "SYN_PRESS_EMPTIES", Quantity: 1})
			},
		},
		{
			name: "wire-retrieve-buried-dig",
			need: map[string]any{"door": "edge wire intake", "type": "retrieve", "payload": "PANEL-A", "source": "SYN_MARKET", "delivery": "SLN_003",
				"setup": "BIN-PA-01 and BIN-PA-02 claimed; BIN-PA-03 moved to the back of Lane_03 with BIN-CLIP-03 in front of it"},
			run: func(g *goldenPlant) {
				h := g.holder("golden-holder-1")
				g.claimBin("BIN-PA-01", h)
				g.claimBin("BIN-PA-02", h)
				// Put a free PANEL-A behind a blocker so the only answer is a dig: move
				// BIN-PA-03 to the back of Lane_03 and a CLIP in front of it.
				g.exec(`UPDATE bins SET node_id = (SELECT id FROM nodes WHERE name = 'SMN_009') WHERE label = 'BIN-PA-03'`)
				g.exec(`UPDATE bins SET node_id = (SELECT id FROM nodes WHERE name = 'SMN_007') WHERE label = 'BIN-CLIP-03'`)
				wire(g, protocol.OrderRequest{OrderUUID: "g-dig", OrderType: protocol.OrderTypeRetrieve,
					PayloadCode: "PANEL-A", SourceNode: "SYN_MARKET", DeliveryNode: "SLN_003", Quantity: 1})
			},
		},
		{
			name: "complex-swap-market-line",
			need: map[string]any{"door": "complex intake", "steps": "pickup SYN_MARKET [PANEL-A] → dropoff ALN_002; pickup ALN_001 → dropoff SYN_MARKET"},
			run: func(g *goldenPlant) {
				complexReq(g, protocol.ComplexOrderRequest{OrderUUID: "g-swap", PayloadCode: "PANEL-A", Quantity: 1,
					ProcessNode: "ALN_001", Steps: []protocol.ComplexOrderStep{
						{Action: "pickup", Node: "SYN_MARKET"},
						{Action: "dropoff", Node: "ALN_002"},
						{Action: "pickup", Node: "ALN_001"},
						{Action: "dropoff", Node: "SYN_MARKET"},
					}})
			},
		},
		{
			name: "complex-empty-pickup-press",
			need: map[string]any{"door": "complex intake", "steps": "pickup SYN_PRESS_EMPTIES (empty) → dropoff PLN_005"},
			run: func(g *goldenPlant) {
				complexReq(g, protocol.ComplexOrderRequest{OrderUUID: "g-empty-leg", PayloadCode: "PANEL-A", Quantity: 1,
					ProcessNode: "PLN_005", Steps: []protocol.ComplexOrderStep{
						{Action: "pickup", Node: "SYN_PRESS_EMPTIES", Empty: true},
						{Action: "dropoff", Node: "PLN_005"},
					}})
			},
		},
		{
			name: "bin-move-to-group",
			need: map[string]any{"door": "bin move", "bin": "BIN-ACT-P1F", "dest": "SYN_PRESS_EMPTIES"},
			run: func(g *goldenPlant) {
				_, err := g.eng.CreateBinMove(engine.BinMoveRequest{
					Selection: engine.BinSelectionByLabel, BinLabel: "BIN-ACT-P1F",
					SourceNodeID: g.node("PLN_001").ID, DestNodeName: "SYN_PRESS_EMPTIES", StationID: "golden",
				})
				g.refused(err)
				g.eng.RunFulfillmentScan()
			},
		},
		{
			name: "bin-move-to-group-with-room",
			need: map[string]any{"door": "bin move", "bin": "BIN-ACT-P1F", "dest": "SYN_MARKET",
				"note": "a group with no declared level: the destination is settled inside ReserveStorageDropoff"},
			run: func(g *goldenPlant) {
				_, err := g.eng.CreateBinMove(engine.BinMoveRequest{
					Selection: engine.BinSelectionByLabel, BinLabel: "BIN-ACT-P1F",
					SourceNodeID: g.node("PLN_001").ID, DestNodeName: "SYN_MARKET", StationID: "golden",
				})
				g.refused(err)
				g.eng.RunFulfillmentScan()
			},
		},
		{
			name: "wire-move-queued-to-full-group",
			need: map[string]any{"door": "edge wire intake", "type": "move", "source": "PLN_001", "delivery": "SYN_CLEAR",
				"setup": "both SYN_CLEAR slots claimed by an order on its way; the move queues against the group, then CLR_002 is freed and the scanner runs"},
			run: func(g *goldenPlant) {
				h := g.holder("golden-holder-1")
				testdb.ClaimSlotForTest(g.t, g.db, g.node("CLR_001").ID, h.ID)
				testdb.ClaimSlotForTest(g.t, g.db, g.node("CLR_002").ID, h.ID)
				wire(g, protocol.OrderRequest{OrderUUID: "g-move-full", OrderType: protocol.OrderTypeMove,
					SourceNode: "PLN_001", DeliveryNode: "SYN_CLEAR", Quantity: 1})
				g.exec(`UPDATE nodes SET claimed_by = NULL WHERE name = 'CLR_002'`)
				g.eng.RunFulfillmentScan()
			},
		},
		{
			name: "bin-move-to-slot",
			need: map[string]any{"door": "bin move", "bin": "BIN-PA-01", "dest": "SMN_007"},
			run: func(g *goldenPlant) {
				_, err := g.eng.CreateBinMove(engine.BinMoveRequest{
					Selection: engine.BinSelectionByLabel, BinLabel: "BIN-PA-01",
					SourceNodeID: g.node("SMN_001").ID, DestNodeName: "SMN_007", StationID: "golden",
				})
				g.refused(err)
				g.eng.RunFulfillmentScan()
			},
		},
		{
			name: "bin-move-to-dead-node",
			need: map[string]any{"door": "bin move", "bin": "BIN-PA-01", "dest": "SMN_007",
				"setup": "SMN_007 disabled (a dead node)",
				"note":  "B2's one predicted behaviour change (v4 §2.5): this refuses at intake once the builder checks the destination"},
			run: func(g *goldenPlant) {
				g.exec(`UPDATE nodes SET enabled = false WHERE name = 'SMN_007'`)
				_, err := g.eng.CreateBinMove(engine.BinMoveRequest{
					Selection: engine.BinSelectionByLabel, BinLabel: "BIN-PA-01",
					SourceNodeID: g.node("SMN_001").ID, DestNodeName: "SMN_007", StationID: "golden",
				})
				g.refused(err)
				g.eng.RunFulfillmentScan()
			},
		},
		{
			name: "bin-move-auto-to-line",
			need: map[string]any{"door": "bin move", "selection": "auto", "source": "SMN_016", "dest": "SLN_004"},
			run: func(g *goldenPlant) {
				_, err := g.eng.CreateBinMove(engine.BinMoveRequest{
					Selection: engine.BinSelectionAuto, SourceNodeID: g.node("SMN_016").ID,
					DestNodeName: "SLN_004", StationID: "golden",
				})
				g.refused(err)
				g.eng.RunFulfillmentScan()
			},
		},
		{
			name: "loader-replenish-single-window",
			need: map[string]any{"door": "loader replenish", "loader": "PLK_X1", "payload": "CLIP", "threshold": 2, "current_uop": 0},
			run:  func(g *goldenPlant) { g.replenish("PLK_X1", "CLIP", "PLK_X1", 2, 0) },
		},
		{
			name: "loader-replenish-multi-window",
			need: map[string]any{"door": "loader replenish", "loader": "PLK_LOADER", "payload": "BRKT", "threshold": 2, "current_uop": 0},
			run:  func(g *goldenPlant) { g.replenish("PLK_LOADER", "BRKT", "", 2, 0) },
		},
		{
			name: "loader-replenish-dedicated",
			need: map[string]any{"door": "loader replenish", "loader": "PLK_HOMES", "payload": "SHIM", "member": "PLK_H1",
				"threshold": 2, "current_uop": 0, "setup": "BIN-SHIM-01 taken off its home PLK_H1 to SLN_011"},
			run: func(g *goldenPlant) {
				g.exec(`UPDATE bins SET node_id = (SELECT id FROM nodes WHERE name = 'SLN_011') WHERE label = 'BIN-SHIM-01'`)
				g.replenish("PLK_HOMES", "SHIM", "PLK_H1", 2, 0)
			},
		},
		{
			name: "carried-bin-recovery-no-robot",
			need: map[string]any{"door": "carried-bin recovery", "bin": "BIN-STUD-03",
				"setup":    "the bin is on a robot's deck; the robot cache is empty because the engine's refresh loop is stopped",
				"coverage": "the refusal only: resolveCarriedBinDestination needs a live robot and is pinned by engine's recovery tests, not here"},
			run: func(g *goldenPlant) {
				carrier := bins.CarrierNodePrefix + "AMR-G1"
				g.exec(`INSERT INTO nodes (name, is_synthetic, enabled) VALUES ($1, true, true)`, carrier)
				g.exec(`UPDATE bins SET node_id = (SELECT id FROM nodes WHERE name = $1) WHERE label = 'BIN-STUD-03'`, carrier)
				_, _, err := g.eng.RecoverCarriedBin(g.bin("BIN-STUD-03").ID, "golden")
				g.refused(err)
			},
		},
		{
			name: "level-keeper-press-empties",
			need: map[string]any{"door": "level keeper", "group": "SYN_PRESS_EMPTIES",
				"setup": "BIN-PEB-01..03 moved out to PLN_005, PLN_006 and SLN_010, so STANDARD is 1 of 4 wanted"},
			run: func(g *goldenPlant) {
				if v := g.db.GetNodeProperty(g.node("SYN_PRESS_EMPTIES").ID, nodes.PropMaintainEnabled); v != "on" {
					g.t.Fatalf("fixture: the demo seeds SYN_PRESS_EMPTIES with maintain_enabled=%q, want on", v)
				}
				for _, m := range [][2]string{{"BIN-PEB-01", "PLN_005"}, {"BIN-PEB-02", "PLN_006"}, {"BIN-PEB-03", "SLN_010"}} {
					g.exec(`UPDATE bins SET node_id = (SELECT id FROM nodes WHERE name = $2) WHERE label = $1`, m[0], m[1])
				}
				g.eng.Maintainer().Tick()
				g.eng.RunFulfillmentScan()
			},
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			g := newGoldenPlant(t)
			g.recordBirths()
			since := g.count(`SELECT COALESCE(MAX(id), 0) FROM outbox`)
			c.run(g)
			golden(t, "doors/"+c.name, c.need, map[string]any{
				"refusals": g.refusals, "events": g.events, "replies": g.replies(since),
				"orders": g.orders(), "fleet": g.fleet(),
			})
			// What the door wrote at birth, beside what it decided
			// (planner_births_docker_test.go): B2's differential.
			golden(t, "births/"+c.name, c.need, g.births(since))
		})
	}
}

// The census of doors this golden reaches, stated so a reader can check it
// against Appendix A of the v4 design without reading every case.
var _ = strings.Join([]string{
	"edge wire intake: wire-* (retrieve, retrieve_empty, store, move, and the buried retrieve that plans a dig)",
	"complex intake: complex-* (a swap, and an empty-carrier pickup)",
	"compound children: wire-retrieve-buried-dig (the dig's legs)",
	"bin move: bin-move-*",
	"level keeper: level-keeper-press-empties",
	"loader replenish: loader-replenish-* (single-window, multi-window, dedicated positions)",
	"carried-bin recovery: carried-bin-recovery-no-robot (the refusal only; see the case); its success case is " +
		"engine's TestB2Pin_RecoveryBirth, since this plant's robot cache is stopped",
}, "\n")
