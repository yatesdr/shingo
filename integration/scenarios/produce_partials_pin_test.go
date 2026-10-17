// What every door that lifts a carrier off a produce line does with it today,
// by what the carrier holds, end to end across the two modules.
//
// A produce line's carrier leaves full, partial, empty (its part bound,
// nothing made) or bare (nothing bound). Each row runs one door on a fresh
// cell and reads Core afterwards: where the carrier stands, its count and
// part, the count ledger rows written for it from the door on, and the CMS
// delta booked for it (the outbound destination is a tagged storeroom and the
// part's template has one line at two parts a cycle, so a booking is visible).
//
// Each row carries two answers. today is the tree before produce partials
// (contents decide, in Core); after is the answer under it. The test asserts
// after; every row whose after differs from today names its ruling.
//
//go:build docker

package scenarios

import (
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"

	corematerial "shingocore/material"
	"shingocore/store/audit"
	corebins "shingocore/store/bins"
	corenodes "shingocore/store/nodes"
	corepayloads "shingocore/store/payloads"

	edgeengine "shingoedge/engine"
	edgemessaging "shingoedge/messaging"
	storeorders "shingoedge/store/orders"
	edgeuop "shingoedge/uop"
)

type ppClass string

const (
	ppFull    ppClass = "full"
	ppPartial ppClass = "partial"
	ppEmpty   ppClass = "empty"
	ppBare    ppClass = "bare"
)

// ppCount is the line cache and Core's count for each class.
var ppCount = map[ppClass]int{ppFull: 40, ppPartial: 17, ppEmpty: 0, ppBare: 0}

// ppCell is a plain produce cell running style A, its line's carrier set to a
// class, the outbound destination a CMS boundary.
func ppCell(t *testing.T, mode protocol.SwapMode, class ppClass, finishLater bool) *ksrCell {
	t.Helper()
	return ppCellOf(t, ksrOpts{role: protocol.ClaimRoleProduce, mode: mode, plain: true}, class, finishLater)
}

// ppCellOf is ppCell for a cell of any shape (a sequential pair, say).
func ppCellOf(t *testing.T, o ksrOpts, class ppClass, finishLater bool) *ksrCell {
	t.Helper()
	mode := o.mode
	o.mirror = finishLater
	c := newKsrCell(t, o)
	if finishLater {
		c.finishLater()
	}
	c.runningCell("setup")
	db := c.core.eng.DB()
	dest, err := db.GetNodeByName(ksrDest)
	mustNil(t, err, "outbound destination")
	mustNil(t, db.SetNodeProperty(dest.ID, corematerial.CMSStoreroomProperty, "SYN1"), "tag the outbound as a storeroom")
	pl, err := db.GetPayloadByCode(ksrPartA)
	mustNil(t, err, "payload A")
	mustNil(t, db.ReplacePayloadManifest(pl.ID, []corepayloads.Line{{PartNumber: "KSR-PA-PART", PartsPerCycle: 2}}),
		"A's template")

	if mode == protocol.SwapModeTwoRobotPressIndex {
		// A produce press holds an empty on deck, not a full.
		_, err = db.DB.Exec(`UPDATE bins SET payload_code='', manifest=NULL, uop_remaining=0,
			manifest_confirmed=false WHERE label='KSR-A-DECK'`)
		mustNil(t, err, "an empty on deck")
	}
	lb := c.lineBin()
	n := ppCount[class]
	if class == ppBare {
		_, err = db.DB.Exec(`UPDATE bins SET payload_code='', manifest=NULL, uop_remaining=0,
			manifest_confirmed=false, loaded_at=NULL WHERE id=$1`, lb)
		mustNil(t, err, "a bare carrier on the line")
	} else {
		// As the first produce tick binds it: the part and its template's lines,
		// unconfirmed until a release-time ingest confirms it.
		mustNil(t, db.SetBinManifest(lb, `{"items":[{"part_number":"KSR-PA-PART"}]}`, ksrPartA, n), "the line's count")
	}
	mustNil(t, c.edge.DB.SetProcessNodeRuntime(c.nodeID, &c.claimA, n), "the line's cache")
	mustNil(t, c.edge.DB.SetProcessNodeActiveBinID(c.nodeID, &lb), "the seat holds the line's carrier")
	// The Edge's inventory sink, as a plant runs it (the test Edge leaves it
	// unwired): the pickup clears the seat through it.
	c.edge.Engine.SetInventoryDeltaSink(edgeuop.New(c.edge.DB, "edge.test", c.edge.DB, c.edge.DB))
	return c
}

func (c *ksrCell) lineBin() int64 {
	c.t.Helper()
	bins, err := c.core.eng.DB().ListBins()
	mustNil(c.t, err, "bins")
	for _, b := range bins {
		if b.Label == "KSR-LINE-BIN" {
			return b.ID
		}
	}
	c.t.Fatal("no line bin")
	return 0
}

// ppMark is where the ledger and the CMS book stood before the door.
type ppMark struct{ ledger, cms int64 }

func (c *ksrCell) ppMark() ppMark {
	c.t.Helper()
	var m ppMark
	mustNil(c.t, c.core.eng.DB().DB.QueryRow(`SELECT COALESCE(MAX(id),0) FROM bin_uop_ledger`).Scan(&m.ledger), "ledger mark")
	mustNil(c.t, c.core.eng.DB().DB.QueryRow(`SELECT COALESCE(MAX(id),0) FROM cms_transactions`).Scan(&m.cms), "cms mark")
	return m
}

// ppSeen is where the line's carrier ended and what was written for it since
// the mark: "at=<group or node> uop=<n> part=<code> ledger=<ops> cms=<sum>".
func (c *ksrCell) ppSeen(m ppMark) string {
	c.t.Helper()
	db := c.core.eng.DB()
	bin, err := db.GetBin(c.lineBin())
	mustNil(c.t, err, "line bin")
	at := c.nodeName(bin.NodeID)
	if bin.NodeID != nil {
		if n, err := db.GetNode(*bin.NodeID); err == nil && n.ParentID != nil {
			at = c.nodeName(n.ParentID)
		}
	}
	rows, err := audit.ListBinUOPByBin(db.DB, bin.ID, 500, 0)
	mustNil(c.t, err, "ledger")
	var ops []string
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].ID > m.ledger {
			ops = append(ops, rows[i].Op)
		}
	}
	var cms int64
	mustNil(c.t, db.DB.QueryRow(`SELECT COALESCE(SUM(delta),0) FROM cms_transactions WHERE bin_id=$1 AND id > $2`,
		bin.ID, m.cms).Scan(&cms), "cms rows")
	part := bin.PayloadCode
	if part == "" {
		part = "-"
	}
	return fmt.Sprintf("at=%s uop=%d part=%s ledger=%s cms=%d", at, bin.UOPRemaining, part, ppOps(ops), cms)
}

// ppOps is the ledger ops in order, the produce ticks' and the lift's rows
// dropped (they say nothing about where the count came from).
func ppOps(ops []string) string {
	var out []string
	for _, o := range ops {
		if o == "lifted" {
			continue
		}
		out = append(out, o)
	}
	if len(out) == 0 {
		return "-"
	}
	return strings.Join(out, ",")
}

type ppRow struct {
	door string
	// finish: claim A names a partial home (KSR-HOME), mirrored to Core.
	finish       bool
	mode         protocol.SwapMode
	class        ppClass
	today, after string
}

func (r ppRow) name() string {
	n := fmt.Sprintf("%s/%s/%s", r.door, r.mode, r.class)
	if r.finish {
		n += "/finish-later"
	}
	return n
}

// ppRun runs one row's door on a fresh cell and returns what Core shows.
func ppRun(t *testing.T, r ppRow) string {
	c := ppCell(t, r.mode, r.class, r.finish)
	m := c.ppMark()
	switch r.door {
	case "request", "request-spb9":
		if r.class == ppEmpty || r.class == ppBare {
			// A line with nothing made: the request is expected refused, and a
			// request that is not is reported, not driven.
			if _, err := c.edge.Engine.RequestProduceSwap(c.nodeID, 0); err != nil {
				return "refused"
			}
			return "accepted"
		}
		if r.door == "request-spb9" {
			nine := 9
			c.disp = &edgeengine.ReleaseDisposition{Mode: edgeuop.DispositionSendPartialBack, PartialCount: &nine, CalledBy: "pp"}
		}
		c.swap(r.name(), c.operatorRequest)
	case "material", "material-count9":
		release := func() (*storeorders.Order, error) { return c.edge.Engine.ReleaseNodeEmpty(c.nodeID) }
		if r.door == "material-count9" {
			release = func() (*storeorders.Order, error) { return c.edge.Engine.ReleaseNodeWithRemainingUOP(c.nodeID, 1, 9) }
		}
		o, err := release()
		if err != nil || o == nil {
			return fmt.Sprintf("refused (%v)", err)
		}
		rel := *o
		c.eventually("the release with the fleet", func() bool { return c.coreOf(rel).VendorOrderID != "" })
		c.drive(c.coreOf(rel), "RUNNING")
		c.liftAt(c.coreOf(rel), ksrLine, true)
		c.drive(c.coreOf(rel), "FINISHED")
		c.eventually("the release done at both ends", func() bool {
			if c.edgeRow(rel.ID).Status == protocol.StatusDelivered {
				mustNil(c.t, c.edge.Engine.OrderManager().ConfirmDelivery(rel.ID, 1), "confirm")
			}
			return protocol.IsTerminal(c.edgeRow(rel.ID).Status) && protocol.IsTerminal(c.coreOf(rel).Status)
		})
	case "changeover", "changeover-curtained":
		if r.door == "changeover-curtained" {
			_, err := c.edge.DB.Exec(`UPDATE process_nodes SET curtain_enabled = 1 WHERE id = ?`, c.nodeID)
			mustNil(t, err, "a curtained line")
		}
		before := c.lastID()
		if _, err := c.edge.Engine.StartProcessChangeover(c.processID, c.styleB, "pp", ""); err != nil {
			return fmt.Sprintf("refused (%v)", err)
		}
		c.tick()
		c.runChangeover(r.name(), before, ksrLine)
	default:
		t.Fatalf("unknown door %q", r.door)
	}
	c.quiet()
	return c.ppSeen(m)
}

// The answers, by door. A full or partial carrier off a staged produce leg
// lands at the claim's outbound whatever it holds, its count written by the
// release-time ingest (set_for_production, manifest_confirmed) and booked
// into the storeroom at two parts a cycle. A line with nothing made refuses
// the REQUEST. The Material page's move carries no count to Core at all (see
// its rows).
const (
	ppAt40   = "at=KSR-DEST uop=40 part=KSR-PA ledger=set_for_production,manifest_confirmed cms=80"
	ppAt17   = "at=KSR-DEST uop=17 part=KSR-PA ledger=set_for_production,manifest_confirmed cms=34"
	ppAt0    = "at=KSR-DEST uop=0 part=KSR-PA ledger=- cms=0"
	ppAtBare = "at=KSR-DEST uop=0 part=- ledger=- cms=0"
	// Departed with no release-time ingest: the count is the streamed one,
	// unconfirmed, and still booked.
	ppNoIngest40 = "at=KSR-DEST uop=40 part=KSR-PA ledger=- cms=80"
	ppNoIngest17 = "at=KSR-DEST uop=17 part=KSR-PA ledger=- cms=34"
)

func ppRows() []ppRow {
	var rows []ppRow
	for _, mode := range protocol.ConfigurableSwapModes() {
		if mode == protocol.SwapModeManualSwap {
			continue
		}
		rows = append(rows,
			ppRow{door: "request", mode: mode, class: ppFull, today: ppAt40, after: ppAt40},
			ppRow{door: "request", mode: mode, class: ppPartial, today: ppAt17, after: ppAt17},
			ppRow{door: "request", mode: mode, class: ppEmpty, today: "refused", after: "refused"},
			ppRow{door: "request", mode: mode, class: ppBare, today: "refused", after: "refused"},
		)
		if mode == protocol.SwapModeSequential {
			// A one-position sequential cell plans no changeover leg (the
			// planner asks for the partner); the sequential changeover leg is
			// pinned in the Edge's changeover tests.
			continue
		}
		today40, today17 := ppAt40, ppAt17
		if mode == protocol.SwapModeTwoRobotPressIndex {
			// Today a press changing carrier type lifts each position on a leg
			// that is never station-released, so no ingest ships. After: every
			// lift off a line position goes through its release (the one door).
			today40, today17 = ppNoIngest40, ppNoIngest17
		}
		rows = append(rows,
			ppRow{door: "changeover", mode: mode, class: ppFull, today: today40, after: ppAt40},
			ppRow{door: "changeover", mode: mode, class: ppPartial, today: today17, after: ppAt17},
		)
		if mode == protocol.SwapModeTwoRobotPressIndex {
			// A curtained press: the curtain's wait was the release already.
			rows = append(rows, ppRow{door: "changeover-curtained", mode: mode, class: ppPartial, today: ppAt17, after: ppAt17})
		}
	}
	tr := protocol.SwapModeTwoRobot
	rows = append(rows,
		ppRow{door: "changeover", mode: tr, class: ppEmpty, today: ppAt0, after: ppAt0},
		ppRow{door: "changeover", mode: tr, class: ppBare, today: ppAtBare, after: ppAtBare},
		// The release modal's SEND PARTIAL BACK with the operator's count of 9:
		// a departing produce leg releases with no count (the ingest is the
		// bin's manifest), so Core keeps the line's 17.
		ppRow{door: "request-spb9", mode: tr, class: ppPartial, today: ppAt17, after: ppAt17},
		// The Material page: the Edge sends the line's count on the move. Today
		// Core's extractRemainingUOP read it from a Data wrapper an order
		// request's envelope does not have, so the claim was a plain one, no
		// sync and no clear (FLAT BUG). After: the count lands, synced onto the
		// carrier at its claim, and a count of 0 clears it.
		ppRow{door: "material", mode: tr, class: ppFull, today: ppNoIngest40,
			after: "at=KSR-DEST uop=40 part=KSR-PA ledger=sync_uop_and_claim cms=80"},
		ppRow{door: "material", mode: tr, class: ppPartial, today: ppNoIngest17,
			after: "at=KSR-DEST uop=17 part=KSR-PA ledger=sync_uop_and_claim cms=34"},
		ppRow{door: "material", mode: tr, class: ppEmpty, today: ppAt0,
			after: "at=KSR-DEST uop=0 part=- ledger=clear_and_claim cms=0"},
		ppRow{door: "material", mode: tr, class: ppBare, today: ppAtBare,
			after: "at=KSR-DEST uop=0 part=- ledger=clear_and_claim cms=0"},
		// The operator types 9 at the Material page: today Core kept the
		// stream's 17; after, the operator's count lands.
		ppRow{door: "material-count9", mode: tr, class: ppPartial, today: ppNoIngest17,
			after: "at=KSR-DEST uop=9 part=KSR-PA ledger=sync_uop_and_claim cms=18"},
	)
	rows = append(rows, ppFinishLaterRows()...)
	return rows
}

// The finish-later claim (ruling: contents decide, in Core). A full leaves for
// the outbound as it always did; an unfinished carrier goes to the claim's
// partial home with its count, by every lifting door: the REQUEST swap in every
// mode, the changeover (curtained or not), the Material page with the count the
// line holds or the one the operator types. Today each of them sent it to the
// outbound.
func ppFinishLaterRows() []ppRow {
	const (
		home17     = "at=KSR-HOME uop=17 part=KSR-PA ledger=set_for_production,manifest_confirmed cms=0"
		homeMove17 = "at=KSR-HOME uop=17 part=KSR-PA ledger=sync_uop_and_claim cms=0"
		homeMove9  = "at=KSR-HOME uop=9 part=KSR-PA ledger=sync_uop_and_claim cms=0"
	)
	var rows []ppRow
	for _, mode := range protocol.ConfigurableSwapModes() {
		if mode == protocol.SwapModeManualSwap {
			continue
		}
		rows = append(rows,
			ppRow{door: "request", mode: mode, class: ppFull, finish: true, today: ppAt40, after: ppAt40},
			ppRow{door: "request", mode: mode, class: ppPartial, finish: true, today: ppAt17, after: home17},
		)
		if mode != protocol.SwapModeSequential {
			today := ppAt17
			if mode == protocol.SwapModeTwoRobotPressIndex {
				today = ppNoIngest17
			}
			rows = append(rows, ppRow{door: "changeover", mode: mode, class: ppPartial, finish: true, today: today, after: home17})
		}
	}
	tr := protocol.SwapModeTwoRobot
	return append(rows,
		ppRow{door: "changeover-curtained", mode: protocol.SwapModeTwoRobotPressIndex, class: ppPartial, finish: true,
			today: ppAt17, after: home17},
		ppRow{door: "material", mode: tr, class: ppPartial, finish: true, today: ppNoIngest17, after: homeMove17},
		ppRow{door: "material-count9", mode: tr, class: ppPartial, finish: true, today: ppNoIngest17, after: homeMove9},
	)
}

func TestProducePartialsPin_LiftDoors(t *testing.T) {
	for _, r := range ppRows() {
		r := r
		t.Run(r.name(), func(t *testing.T) {
			got := ppRun(t, r)
			t.Logf("PIN %-40s %s", r.name(), got)
			if got != r.after {
				t.Errorf("%s\n got  %s\n want %s\n (today %s)", r.name(), got, r.after, r.today)
			}
		})
	}
}

// The Material page's count on a CONSUME line: the operator releases the
// line's bin as empty. Today Core never read the count (the Data-wrapper bug)
// and the bin landed still holding 40 of its part; after, a count of 0 clears
// it at its claim.
func TestProducePartialsPin_MaterialPageCountOnAConsumeLine(t *testing.T) {
	c := newKsrCell(t, ksrOpts{role: protocol.ClaimRoleConsume, mode: protocol.SwapModeTwoRobot, plain: true})
	c.runningCell("setup")
	lb := c.lineBin()
	mustNil(t, c.edge.DB.SetProcessNodeActiveBinID(c.nodeID, &lb), "the seat holds the line's bin")
	c.edge.Engine.SetInventoryDeltaSink(edgeuop.New(c.edge.DB, "edge.test", c.edge.DB, c.edge.DB))
	m := c.ppMark()
	o, err := c.edge.Engine.ReleaseNodeWithRemainingUOP(c.nodeID, 1, 0)
	if err != nil || o == nil {
		t.Fatalf("release refused: %v", err)
	}
	rel := *o
	c.eventually("the release with the fleet", func() bool { return c.coreOf(rel).VendorOrderID != "" })
	c.drive(c.coreOf(rel), "RUNNING")
	c.liftAt(c.coreOf(rel), ksrLine, true)
	c.drive(c.coreOf(rel), "FINISHED")
	c.eventually("the release done at both ends", func() bool {
		if c.edgeRow(rel.ID).Status == protocol.StatusDelivered {
			mustNil(c.t, c.edge.Engine.OrderManager().ConfirmDelivery(rel.ID, 1), "confirm")
		}
		return protocol.IsTerminal(c.edgeRow(rel.ID).Status) && protocol.IsTerminal(c.coreOf(rel).Status)
	})
	c.quiet()
	got := c.ppSeen(m)
	today := "at=KSR-DEST uop=40 part=KSR-PA ledger=- cms=0"
	after := "at=KSR-DEST uop=0 part=- ledger=clear_and_claim cms=0"
	t.Logf("PIN material/consume/released-empty %s", got)
	if got != after {
		t.Errorf("got  %s\nwant %s (today %s)", got, after, today)
	}
}

// ksrHome is the partial home a finish-later claim names: a group of slots.
const ksrHome = "KSR-HOME"

// finishLater makes claim A a finish-later claim: Core gets a home group of
// three slots, the Edge's claim names it as its partial destination, and the
// claims reach Core's mirror over the wire.
func (c *ksrCell) finishLater() {
	c.t.Helper()
	db := c.core.eng.DB()
	ngrp, err := db.GetNodeTypeByCode("NGRP")
	mustNil(c.t, err, "NGRP type")
	home := &corenodes.Node{Name: ksrHome, IsSynthetic: true, NodeTypeID: &ngrp.ID, Enabled: true}
	mustNil(c.t, db.CreateNode(home), "the partial home")
	for i := 1; i <= 3; i++ {
		mustNil(c.t, db.CreateNode(&corenodes.Node{Name: fmt.Sprintf("%s-%d", ksrHome, i), ParentID: &home.ID, Enabled: true}),
			"a home slot")
	}
	_, err = c.edge.DB.Exec(`UPDATE style_node_claims SET partial_destination = ? WHERE id = ?`, ksrHome, c.claimA)
	mustNil(c.t, err, "claim A finishes later")
	mustNil(c.t, edgemessaging.NewPlantClaimsPublisher(c.edge.DB, "edge.test", 0).PublishChanged(c.processID), "publish")
	c.settle()
	got, ambiguous, err := db.PartialHomeFor(ksrLine, ksrPartA)
	mustNil(c.t, err, "the mirror's partial home")
	if got != ksrHome || ambiguous {
		c.t.Fatalf("Core's mirror names partial home %q (ambiguous %v), want %s", got, ambiguous, ksrHome)
	}
}

// ppHeldPartial stands a held partial of A in the cell's home: n of 40, landed
// there through the real arrival, so it is held from now.
func (c *ksrCell) ppHeldPartial(label string, n int) {
	c.t.Helper()
	db := c.core.eng.DB()
	slot, err := db.GetNodeByName(ksrHome + "-1")
	mustNil(c.t, err, "a home slot")
	line, err := db.GetNodeByName(ksrLine)
	mustNil(c.t, err, "the line")
	b := &corebins.Bin{BinTypeID: c.typeA.ID, Label: label, NodeID: &line.ID, Status: "available"}
	mustNil(c.t, db.CreateBin(b), "the partial")
	mustNil(c.t, db.SetBinManifest(b.ID, `{"items":[{"part_number":"KSR-PA-PART"}]}`, ksrPartA, n), "its count")
	_, err = c.core.eng.BinService().ApplyArrival(b.ID, slot.ID, false, nil, 0)
	mustNil(c.t, err, "it lands in its home")
}

// TestProducePartials_TheLineFinishesItsPartial: a finish-later cell holding a
// partial of its part in its home. A REQUEST swap, in every mode, sends the
// line's unfinished carrier home and brings the held one back to be finished:
// the carrier that lands on the line is the home's partial with its count, not
// a fresh empty, and the station counts from that count: the landing's, as the
// Edge unit pins it too (TestProducePartials_AReturnedPartialCountsFromItsCount).
func TestProducePartials_TheLineFinishesItsPartial(t *testing.T) {
	for _, mode := range protocol.ConfigurableSwapModes() {
		if mode == protocol.SwapModeManualSwap {
			continue
		}
		mode := mode
		t.Run(string(mode), func(t *testing.T) {
			c := ppCell(t, mode, ppPartial, true)
			c.ppHeldPartial("KSR-HELD", 22)
			m := c.ppMark()
			c.swap("finish the held partial", c.operatorRequest)
			c.quiet()
			db := c.core.eng.DB()
			bins, err := db.ListBins()
			mustNil(t, err, "bins")
			var onLine []string
			for _, b := range bins {
				if c.nodeName(b.NodeID) == ksrLine {
					onLine = append(onLine, fmt.Sprintf("%s:%d", b.Label, b.UOPRemaining))
				}
			}
			got := fmt.Sprintf("line=%v old=%s", onLine, c.ppSeen(m))
			want := "line=[KSR-HELD:22] old=at=KSR-HOME uop=17 part=KSR-PA ledger=set_for_production,manifest_confirmed cms=0"
			wantCount := 22
			if mode == protocol.SwapModeTwoRobotPressIndex {
				// The press indexes its deck forward: the deck's empty goes to the
				// line and the fetched carrier refills the deck behind it.
				want = "line=[KSR-A-DECK:0] old=at=KSR-HOME uop=17 part=KSR-PA ledger=set_for_production,manifest_confirmed cms=0"
				wantCount = 0
			}
			t.Logf("PIN finish/%s %s", mode, got)
			if got != want {
				t.Errorf("got  %s\nwant %s", got, want)
			}
			rt, err := c.edge.DB.GetProcessNodeRuntime(c.nodeID)
			mustNil(t, err, "the line's seat")
			if rt.RemainingUOPCached != wantCount {
				t.Errorf("the station counts %d on the line's carrier, want %d", rt.RemainingUOPCached, wantCount)
			}
		})
	}
}

// TestProducePartials_ASequentialPairSendsItsPartialHome: a sequential line of
// two positions, finish-later. A partial lifted off the line goes to the home,
// by the REQUEST and by the changeover: the one door holds on a multi-position
// cell too, through Core's divert at the leg's release.
func TestProducePartials_ASequentialPairSendsItsPartialHome(t *testing.T) {
	const home17 = "at=KSR-HOME uop=17 part=KSR-PA ledger=set_for_production,manifest_confirmed cms=0"
	for _, door := range []string{"request", "changeover"} {
		door := door
		t.Run(door, func(t *testing.T) {
			c := ppCellOf(t, ksrOpts{role: protocol.ClaimRoleProduce, mode: protocol.SwapModeSequential, plain: true, pair: true},
				ppPartial, true)
			m := c.ppMark()
			switch door {
			case "request":
				c.swap("the pair's swap", c.operatorRequest)
			case "changeover":
				before := c.lastID()
				if _, err := c.edge.Engine.StartProcessChangeover(c.processID, c.styleB, "pp", ""); err != nil {
					t.Fatalf("changeover: %v", err)
				}
				c.tick()
				c.runChangeover("the pair's changeover", before, ksrLine, ksrPair)
			}
			c.quiet()
			got := c.ppSeen(m)
			t.Logf("PIN sequential-pair/%s %s", door, got)
			if got != home17 {
				t.Errorf("got  %s\nwant %s", got, home17)
			}
		})
	}
}
