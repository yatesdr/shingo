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
	corepayloads "shingocore/store/payloads"

	edgeengine "shingoedge/engine"
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
func ppCell(t *testing.T, mode protocol.SwapMode, class ppClass) *ksrCell {
	t.Helper()
	c := newKsrCell(t, ksrOpts{role: protocol.ClaimRoleProduce, mode: mode, plain: true})
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
	door         string
	mode         protocol.SwapMode
	class        ppClass
	today, after string
}

func (r ppRow) name() string { return fmt.Sprintf("%s/%s/%s", r.door, r.mode, r.class) }

// ppRun runs one row's door on a fresh cell and returns what Core shows.
func ppRun(t *testing.T, r ppRow) string {
	c := ppCell(t, r.mode, r.class)
	m := c.ppMark()
	switch r.door {
	case "request", "request-spb9":
		if r.class == ppEmpty || r.class == ppBare {
			// A line with nothing made: the request is expected refused, and a
			// request that is not is reported, not driven.
			if _, err := c.edge.Engine.RequestProduceSwap(c.nodeID); err != nil {
				return "refused"
			}
			return "accepted"
		}
		if r.door == "request-spb9" {
			nine := 9
			c.disp = &edgeengine.ReleaseDisposition{Mode: edgeuop.DispositionSendPartialBack, PartialCount: &nine, CalledBy: "pp"}
		}
		c.swap(r.name(), c.operatorRequest)
	case "material":
		o, err := c.edge.Engine.ReleaseNodeEmpty(c.nodeID)
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
	case "changeover":
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
		at40, at17 := ppAt40, ppAt17
		if mode == protocol.SwapModeTwoRobotPressIndex {
			// The press's changeover evac leaves the line on a leg that is
			// never station-released, so no ingest ships.
			at40, at17 = ppNoIngest40, ppNoIngest17
		}
		rows = append(rows,
			ppRow{door: "changeover", mode: mode, class: ppFull, today: at40, after: at40},
			ppRow{door: "changeover", mode: mode, class: ppPartial, today: at17, after: at17},
		)
	}
	tr := protocol.SwapModeTwoRobot
	rows = append(rows,
		ppRow{door: "changeover", mode: tr, class: ppEmpty, today: ppAt0, after: ppAt0},
		ppRow{door: "changeover", mode: tr, class: ppBare, today: ppAtBare, after: ppAtBare},
		// The release modal's SEND PARTIAL BACK with the operator's count of 9:
		// a departing produce leg releases with no count (the ingest is the
		// bin's manifest), so Core keeps the line's 17.
		ppRow{door: "request-spb9", mode: tr, class: ppPartial, today: ppAt17, after: ppAt17},
		// The Material page: the Edge sends the line's count on the move and
		// Core's extractRemainingUOP reads it from a wrapper the envelope does
		// not have, so the claim is a plain one, no sync and no clear, and the
		// carrier lands with whatever count the stream left, unconfirmed.
		ppRow{door: "material", mode: tr, class: ppFull, today: ppNoIngest40, after: ppNoIngest40},
		ppRow{door: "material", mode: tr, class: ppPartial, today: ppNoIngest17, after: ppNoIngest17},
		ppRow{door: "material", mode: tr, class: ppEmpty, today: ppAt0, after: ppAt0},
		ppRow{door: "material", mode: tr, class: ppBare, today: ppAtBare, after: ppAtBare},
	)
	return rows
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
