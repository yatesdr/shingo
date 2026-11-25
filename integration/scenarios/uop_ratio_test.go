//go:build docker

// The owner's example end to end across the two modules, under THE RULE
// (owner, 2026-11-19; protocol/count.go): a UOP is one assembly cycle's worth.
// A bin's count is what is physically in the carrier, parts for a payload of
// one manifest line and kits for a payload of several, and UOP is never
// stored: it is read, count ÷ ratio, with the parts left over beside it. A
// press adds parts; an assembly's consume cycle draws step × ratio; a ratio
// change rewrites no stored number; CMS is booked the count as is for a
// one-line payload and count × the line's ratio for each line of a kit.
//
// One Core and one Edge. The press is newKsrCell's plain two-robot produce
// cell, its outbound destination a CMS storeroom as ppCellOf tags it, on the
// catid_4x2 cell's bus (w6Cell.wireBus), which carries the Edge's draws
// (bin_uop_delta) to Core. Two consume processes join it on the same Edge, an
// assembly of the press's part and an assembly of a kit, both fed from the
// storeroom and sending their carriers back to it. Templates are written
// through Core's payload service, as the Edit Payload page writes them, and
// reach the Edge by the heartbeat's catalog feed (uopCell.catalogBeat). Every
// count moves by a real door: the counter's deltas (the Edge's tick handler),
// the REQUEST swap and its release-time ingest, the plain delivery, the Edge's
// record-count door (an ask Core answers), the Material page's release and
// Core's Load Payload door.
//
// At every hop the test reads both books, the Edge seat's remaining_uop_cached
// and Core's bins.uop_remaining, the reading each side's own template makes of
// them (protocol.SplitUOP over protocol.UOPRatio: no screen runs here), and
// the rows Core booked for CMS since the hop began (cms_transactions, what the
// poster posts: one row per manifest line, signed by direction).

package scenarios

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/router"

	coredomain "shingocore/domain"
	corematerial "shingocore/material"
	coremessaging "shingocore/messaging"
	coreservice "shingocore/service"
	"shingocore/store/audit"
	corebins "shingocore/store/bins"
	corenodes "shingocore/store/nodes"
	corepayloads "shingocore/store/payloads"

	"shingoedge/domain"
	edgeengine "shingoedge/engine"
	"shingoedge/store/catalog"
	"shingoedge/store/processes"
	edgeuop "shingoedge/uop"
)

const (
	uopPart     = "PANEL"       // the one line of KSR-PA's template
	uopRatio    = 8             // PANELs one assembly cycle takes
	uopRatioNew = 4             // the ratio Core is changed to
	uopPack     = 800           // PANELs in a full bin
	uopAssy     = "KSR-ASSY"    // the PANEL assembly's line
	uopKit      = "KSR-KIT"     // a payload of three lines
	uopKitAssy  = "KSR-KITASSY" // the kit assembly's line
	uopKitLoad  = 60            // kits loaded on the kit's cart
	uopRoom     = "SYN1"        // the storeroom's CMS code
)

// TestUOP_OwnersExample_StoresPartsReadsUOP: PANEL at 8 a cycle with a pack of
// 800. The press pulls a bin at 404 (read 50 UOP + 4) and fills the next to 800
// (100 UOP); CMS is booked the parts. The assembly is brought the 404 bin and
// one cycle draws 8 (396); the operator counts 803 (100 UOP + 3, accepted above
// the pack); the ratio becomes 4 on Core and nothing stored moves while the
// same 803 reads 200 UOP + 3, and the next cycle draws 4 (799), which CMS is
// booked as is when the bin goes back. A kit of lines at 1, 1 and 2 loaded at
// 60 holds 60 kits, read 60 UOP; CMS is booked 60, 60 and 120 for its move;
// one cycle draws 1 kit.
func TestUOP_OwnersExample_StoresPartsReadsUOP(t *testing.T) {
	t.Parallel()
	c := newKsrCell(t, ksrOpts{role: protocol.ClaimRoleProduce, mode: protocol.SwapModeTwoRobot, plain: true, counted: true, rigPush: true})
	c.runningCell("setup")
	w := &w6Cell{ksrCell: c}
	w.wireBus()
	// The Edge's inventory sink, as a plant runs it; w.flush sends its draws to
	// Core in place of the periodic flush.
	w.sink = edgeuop.New(c.edge.DB, "edge.test", c.edge.DB, c.edge.DB)
	c.edge.Engine.SetInventoryDeltaSink(w.sink)
	u := &uopCell{w6Cell: w}
	coreDB := c.core.eng.DB()
	templates := c.core.eng.PayloadService()

	// The catalog feed, routed as the two composition roots route it: the
	// Edge's heartbeat to Core's HandleEdgeHeartbeat, Core's catalog to the
	// Edge's ApplyCatalog, and the payload service's writes to the same data
	// service's change door (cmd/shingocore wires SetFeedsChangedFunc to it).
	// wireBus routes neither subject, so each side's ingestor takes those two
	// first and hands every other envelope on as before.
	feed := coremessaging.NewCoreDataService(coreDB,
		coremessaging.NewCoreHandler(coreDB, nil, "core", "shingo.dispatch", c.core.eng.Dispatcher()),
		coreservice.EpochAnnounce{Topic: "shingo.dispatch", CoreStation: "core"})
	c.core.eng.SetFeedsChangedFunc(feed.FeedsChanged)
	toCore := router.NewSubject()
	router.RegisterSubject(toCore, protocol.SubjectEdgeHeartbeat, feed.HandleEdgeHeartbeat)
	routeFirst(w.coreIn, toCore)
	toEdge := router.NewSubject()
	router.RegisterSubject(toEdge, protocol.SubjectCatalogPayloadsResponse,
		func(_ *protocol.Envelope, r *protocol.CatalogPayloadsResponse) { c.edge.Engine.ApplyCatalog(r) })
	routeFirst(c.edge.Ingestor, toEdge)

	dest, err := coreDB.GetNodeByName(ksrDest)
	mustNil(t, err, "the outbound destination")
	mustNil(t, coreDB.SetNodeProperty(dest.ID, corematerial.CMSStoreroomProperty, uopRoom), "tag the outbound as a storeroom")

	// 1. PANEL at 8 a cycle, a pack of 800: Core's template, saved as the Edit
	// Payload page saves it, and the Edge's copy, by the heartbeat. The
	// fixture's line-side bin already holds a count of the part, so Core warns
	// first (step 6 below pins the warning) and this is the confirmed save.
	pl, err := templates.GetByCode(ksrPartA)
	mustNil(t, err, "KSR-PA")
	pl.UOPCapacity = uopPack
	mustNil(t, templates.UpdateWithSetsSeen(pl, coreservice.PayloadSets{Manifest: &[]coreservice.ManifestLine{
		{PartNumber: uopPart, PartsPerCycle: uopRatio}}}, true), "KSR-PA's template")
	u.catalogBeat("1 the template", ksrPartA, func(e *catalog.CatalogEntry) bool {
		return e.UOPCapacity == uopPack && len(e.Manifest) == 1 && e.Manifest[0].PartNumber == uopPart &&
			protocol.UOPRatio(e.Manifest) == uopRatio
	})
	if r := u.coreRatio(ksrPartA); r != uopRatio {
		t.Fatalf("Core's template gives KSR-PA a ratio of %d, want %d", r, uopRatio)
	}

	// The press's first carrier: the line's bin, emptied, bound at the seat
	// with its generation.
	first := c.lineBin()
	_, err = coreDB.DB.Exec(`UPDATE bins SET payload_code='', manifest=NULL, uop_remaining=0,
		manifest_confirmed=false, loaded_at=NULL WHERE id=$1`, first)
	mustNil(t, err, "an empty carrier on the press")
	empty, err := coreDB.GetBin(first)
	mustNil(t, err, "the press's bin")
	c.bus.PushBins()
	mustNil(t, c.edge.DB.SetProcessNodeRuntime(c.nodeID, &c.claimA, 0), "the press's count")
	mustNil(t, c.edge.DB.SetProcessNodeActiveBinIDAndEpoch(c.nodeID, &first, empty.DeltaEpoch), "the seat holds the bin")

	// 3 (run first, so the oldest bin in the storeroom is this one). The press
	// makes 404 and the operator pulls the bin: 404 parts in both books, read
	// 50 UOP + 4; CMS is booked 404 PANELs at the storeroom.
	tickN(c, c.processID, c.styleA, 404)
	w.flush()
	m := c.ppMark()
	u.hop("3 press made 404, at the pull", c.nodeID, first, ksrPartA, 404, 50, 4, m, "-")
	c.swap("the 404 bin pulled", c.operatorRequest)
	c.quiet()
	u.inStoreroom("the 404 bin pulled", first)
	u.hop("3 pulled to the storeroom", 0, first, ksrPartA, 404, 50, 4, m, uopPart+"+404@"+uopRoom)

	// 2. The press fills its next carrier to 800: 800 in both books, read 100
	// UOP (SplitUOP(800, 8) is 100 and 0); CMS is booked 800, not 800 × 8.
	n, second := u.seat(c.nodeID)
	if second == 0 || second == first || n != 0 {
		c.dump("the press after the pull")
		t.Fatalf("after the pull the press holds bin %d at %d, want a fresh carrier at 0", second, n)
	}
	tickN(c, c.processID, c.styleA, uopPack)
	w.flush()
	m = c.ppMark()
	u.hop("2 press filled 800", c.nodeID, second, ksrPartA, 800, 100, 0, m, "-")
	c.swap("the full bin to the storeroom", c.operatorRequest)
	c.quiet()
	u.inStoreroom("the full bin", second)
	u.hop("2 full bin to the storeroom", 0, second, ksrPartA, 800, 100, 0, m, uopPart+"+800@"+uopRoom)

	// 4. An assembly of PANEL fetches one from the storeroom, the oldest: the
	// 404 bin, booked out of the storeroom as 404 PANELs. One cycle draws 8.
	assyProc, assyNode, assyStyle := u.consumeLine(uopAssy, ksrPartA, uopPack)
	m = c.ppMark()
	if got := u.deliver("the assembly's REQUEST", assyNode); got != first {
		c.dump("the assembly's delivery")
		t.Fatalf("the assembly was brought bin %d, want the 404 bin %d (the storeroom's oldest)", got, first)
	}
	u.hop("4 at the assembly", assyNode, first, ksrPartA, 404, 50, 4, m, uopPart+"-404@"+uopRoom)
	m = c.ppMark()
	tickN(c, assyProc, assyStyle, 1)
	w.flush()
	u.hop("4 one cycle draws 8", assyNode, first, ksrPartA, 396, 49, 4, m, "-")

	// 5. The operator counts 803 at the Edge's record-count door, an ask Core
	// answers: Core takes it above the pack of 800 and notes so in the bin's
	// ledger; 803 in both books, read 100 UOP + 3.
	m = c.ppMark()
	counted := make(chan error, 1)
	go func() { counted <- c.edge.Engine.RecordBinCount(assyNode, 803, "uop-operator") }()
	for waiting := true; waiting; {
		select {
		case err := <-counted:
			mustNil(t, err, "the count of 803 at the line")
			waiting = false
		case <-time.After(10 * time.Millisecond):
			c.bus.PumpAll()
		}
	}
	c.settle()
	u.hop("5 counted 803", assyNode, first, ksrPartA, 803, 100, 3, m, "-")
	if got := u.ledgerOps(m, first); got != "cycle_count,cycle_count_over_capacity" {
		t.Errorf("the count of 803 wrote %s to the bin's ledger, want cycle_count,cycle_count_over_capacity "+
			"(accepted above the pack, with the note)", got)
	}

	// 6. The ratio becomes 4 on Core, through the same save. Core first warns
	// what the change does to the two bins that hold a count, writing nothing;
	// the engineer's confirmed save writes it. No stored number moves, in
	// either book, on either bin; the Edge has the ratio on its next heartbeat;
	// the same 803 reads 200 UOP + 3 and the 800 in the storeroom 200 UOP. The
	// next cycle draws 4.
	m = c.ppMark()
	pl, err = templates.GetByCode(ksrPartA)
	mustNil(t, err, "KSR-PA")
	to4 := coreservice.PayloadSets{Manifest: &[]coreservice.ManifestLine{{PartNumber: uopPart, PartsPerCycle: uopRatioNew}}}
	var unseen *coreservice.RatioChangeUnseen
	if err := templates.UpdateWithSets(pl, to4); !errors.As(err, &unseen) {
		t.Fatalf("6 the ratio to 4, unconfirmed: %v, want Core's warning (RatioChangeUnseen) and nothing written", err)
	}
	t.Logf("6 Core's warning: %s", unseen.Error())
	if ch := unseen.Change; ch.From != uopRatio || ch.To != uopRatioNew || ch.Bins != 2 || len(ch.Rows) != 2 {
		t.Errorf("6 the warning = %+v, want %d to %d over the two counted bins", ch, uopRatio, uopRatioNew)
	}
	for _, r := range unseen.Change.Rows {
		if want := map[int]int{803: 200, 800: 200}[r.Parts]; want == 0 || r.UOPAfter != want || r.UOPBefore != r.Parts/uopRatio {
			t.Errorf("6 the warning's row %+v: want the 803 and the 800, each read before at %d and after at %d", r, uopRatio, uopRatioNew)
		}
	}
	mustNil(t, templates.UpdateWithSetsSeen(pl, to4, true), "KSR-PA's ratio to 4, confirmed")
	u.catalogBeat("6 the ratio to 4", ksrPartA, func(e *catalog.CatalogEntry) bool {
		return protocol.UOPRatio(e.Manifest) == uopRatioNew
	})
	u.hop("6 ratio 8 -> 4, the counted bin", assyNode, first, ksrPartA, 803, 200, 3, m, "-")
	u.hop("6 ratio 8 -> 4, the full bin", 0, second, ksrPartA, 800, 200, 0, m, "-")
	for _, b := range []int64{first, second} {
		if got := u.ledgerOps(m, b); got != "-" {
			t.Errorf("the ratio change wrote %s to bin %d's ledger, want nothing", got, b)
		}
	}
	m = c.ppMark()
	tickN(c, assyProc, assyStyle, 1)
	w.flush()
	u.hop("6 one cycle draws 4", assyNode, first, ksrPartA, 799, 199, 3, m, "-")

	// 8 (the one-line bin at the new ratio). The Material page sends the bin
	// back to the storeroom with the seat's count: CMS is booked 799 PANELs,
	// not 799 × 4.
	m = c.ppMark()
	o, err := c.edge.Engine.ReleaseNodePartial(assyNode, 1)
	if err != nil || o == nil {
		c.dump("the release at the assembly")
		t.Fatalf("the Material page's release at the assembly was refused: %v", err)
	}
	rel := *o
	c.eventually("the release with the fleet", func() bool { return c.coreOf(rel).VendorOrderID != "" })
	c.drive(c.coreOf(rel), "RUNNING")
	c.liftAt(c.coreOf(rel), uopAssy, true)
	c.drive(c.coreOf(rel), "FINISHED")
	c.driveUntil("the release done at both ends", func() bool {
		if c.edgeRow(rel.ID).Status == protocol.StatusDelivered {
			mustNil(t, c.edge.Engine.OrderManager().ConfirmDelivery(rel.ID, 1), "confirm the release")
		}
		return protocol.IsTerminal(c.edgeRow(rel.ID).Status) && protocol.IsTerminal(c.coreOf(rel).Status)
	})
	u.inStoreroom("the released bin", first)
	u.hop("8 released to the storeroom", 0, first, ksrPartA, 799, 199, 3, m, uopPart+"+799@"+uopRoom)

	// 7. A kit: lines A, B and C at 1, 1 and 2 a cycle. Its cart, standing empty
	// in the storeroom, is loaded at 60 at Core's Load Payload door: 60 kits,
	// read 60 UOP (a kit's ratio is 1). The kit assembly fetches it, which CMS
	// is booked 60 × each line's ratio; one cycle draws one kit.
	kitType := &corebins.BinType{Code: "KSR-TK"}
	mustNil(t, coreDB.CreateBinType(kitType), "the kit's carrier type")
	kitLines := []coreservice.ManifestLine{
		{PartNumber: "KIT-A", PartsPerCycle: 1}, {PartNumber: "KIT-B", PartsPerCycle: 1}, {PartNumber: "KIT-C", PartsPerCycle: 2},
	}
	mustNil(t, templates.CreateWithSets(&corepayloads.Payload{Code: uopKit, UOPCapacity: uopKitLoad},
		coreservice.PayloadSets{BinTypeIDs: &[]int64{kitType.ID}, Manifest: &kitLines}), "the kit's template")
	u.catalogBeat("7 the kit's template", uopKit, func(e *catalog.CatalogEntry) bool { return len(e.Manifest) == 3 })
	pairs, err := coreDB.ListPayloadBinTypeMappings()
	mustNil(t, err, "the carrier list")
	var carriers []protocol.PayloadBinTypeInfo
	for _, p := range pairs {
		carriers = append(carriers, protocol.PayloadBinTypeInfo{PayloadCode: p[0], BinTypeCode: p[1]})
	}
	// The carrier rule Core sends with the node list, as newKsrCell applies it.
	c.edge.Engine.SetPayloadBinTypes(carriers)
	cart := &corebins.Bin{BinTypeID: kitType.ID, Label: "KSR-KIT-CART", NodeID: u.freeStoreroomSlot(), Status: "available"}
	mustNil(t, coreDB.CreateBin(cart), "the kit's cart")
	m = c.ppMark()
	load := uopKitLoad
	mustNil(t, c.core.eng.BinService().LoadPayload(cart.ID, uopKit, &load, protocol.DeclaredByPerson), "Load Payload at 60")
	u.hop("7 kit cart loaded at 60", 0, cart.ID, uopKit, 60, 60, 0, m, "-")
	// A Load Payload leaves the manifest unconfirmed, and nothing is sourced
	// from an unconfirmed bin: the bins page's Confirm Manifest, as a person
	// sending the cart to the line presses it.
	loaded, err := coreDB.GetBin(cart.ID)
	mustNil(t, err, "the kit's cart")
	t.Logf("the kit's cart after Load Payload: manifest confirmed %t", loaded.ManifestConfirmed)
	mustNil(t, c.core.eng.BinService().Manifest().Confirm(cart.ID, ""), "Confirm Manifest on the kit's cart")
	u.hop("7 kit cart confirmed", 0, cart.ID, uopKit, 60, 60, 0, m, "-")
	kitProc, kitNode, kitStyle := u.consumeLine(uopKitAssy, uopKit, uopKitLoad)
	m = c.ppMark()
	if got := u.deliver("the kit assembly's REQUEST", kitNode); got != cart.ID {
		c.dump("the kit's delivery")
		t.Fatalf("the kit assembly was brought bin %d, want the kit's cart %d", got, cart.ID)
	}
	u.hop("7 kit at its assembly", kitNode, cart.ID, uopKit, 60, 60, 0, m,
		"KIT-A-60@"+uopRoom+" KIT-B-60@"+uopRoom+" KIT-C-120@"+uopRoom)
	m = c.ppMark()
	tickN(c, kitProc, kitStyle, 1)
	w.flush()
	u.hop("7 one cycle draws 1 kit", kitNode, cart.ID, uopKit, 59, 59, 0, m, "-")

	t.Logf("the owner's example, hop by hop:\n%s", strings.Join(u.hops, "\n"))
}

// uopCell is the press cell on the catid_4x2 cell's bus, and the hops it has
// read.
type uopCell struct {
	*w6Cell
	hops []string
}

// seat is the count an Edge seat holds and the bin it holds (0 for none).
func (u *uopCell) seat(node int64) (count int, bin int64) {
	u.t.Helper()
	rt, err := u.edge.DB.GetProcessNodeRuntime(node)
	mustNil(u.t, err, "the seat")
	if rt.ActiveBinID != nil {
		bin = *rt.ActiveBinID
	}
	return rt.RemainingUOPCached, bin
}

// edgeRatio and coreRatio are the UOP ratio each side's own copy of a
// payload's template gives it.
func (u *uopCell) edgeRatio(code string) int {
	u.t.Helper()
	e, err := u.edge.DB.GetPayloadCatalogByCode(code)
	mustNil(u.t, err, "the Edge's catalog row of "+code)
	return protocol.UOPRatio(e.Manifest)
}

func (u *uopCell) coreRatio(code string) int {
	u.t.Helper()
	svc := u.core.eng.PayloadService()
	p, err := svc.GetByCode(code)
	mustNil(u.t, err, "Core's payload "+code)
	lines, err := svc.ListManifest(p.ID)
	mustNil(u.t, err, "Core's template of "+code)
	return protocol.UOPRatio(coredomain.ProtocolLines(lines))
}

// hop checks one hop's books: the seat at node (0: no seat holds the bin now)
// and Core's bin both hold want, each side's own template reads it as wantUOP
// UOP and wantParts parts, and CMS was booked wantCMS for the bin since m
// (uopCell.cms). It keeps the hop's line for the table.
func (u *uopCell) hop(name string, node, bin int64, code string, want, wantUOP, wantParts int, m ppMark, wantCMS string) {
	u.t.Helper()
	at := "-"
	if node != 0 {
		got, holds := u.seat(node)
		at = fmt.Sprint(got)
		if holds != bin || got != want {
			u.t.Errorf("%s: the Edge seat holds bin %d at %d, want bin %d at %d", name, holds, got, bin, want)
		}
		if uop, parts := protocol.SplitUOP(got, u.edgeRatio(code)); uop != wantUOP || parts != wantParts {
			u.t.Errorf("%s: the Edge reads %d as %d UOP + %d parts, want %d UOP + %d parts",
				name, got, uop, parts, wantUOP, wantParts)
		}
	}
	b, err := u.core.eng.DB().GetBin(bin)
	mustNil(u.t, err, name+": Core's bin")
	if b.UOPRemaining != want || b.PayloadCode != code {
		u.t.Errorf("%s: Core's bin %d holds %d of %q, want %d of %s", name, bin, b.UOPRemaining, b.PayloadCode, want, code)
	}
	uop, parts := protocol.SplitUOP(b.UOPRemaining, u.coreRatio(code))
	if uop != wantUOP || parts != wantParts {
		u.t.Errorf("%s: Core reads %d as %d UOP + %d parts, want %d UOP + %d parts",
			name, b.UOPRemaining, uop, parts, wantUOP, wantParts)
	}
	booked := u.cms(m, bin)
	if booked != wantCMS {
		u.t.Errorf("%s: CMS was booked %s for bin %d, want %s", name, booked, bin, wantCMS)
	}
	line := fmt.Sprintf("%-32s edge=%-4s core=%-4d reads %d UOP + %d parts  cms=%s",
		name, at, b.UOPRemaining, uop, parts, booked)
	u.t.Log("HOP " + line)
	u.hops = append(u.hops, line)
}

// cms is what Core booked for CMS for bin since m, in the order written:
// "<part><signed delta>@<storeroom>" per row, "-" for none.
func (u *uopCell) cms(m ppMark, bin int64) string {
	u.t.Helper()
	rows, err := u.core.eng.DB().DB.Query(`SELECT cat_id, delta, storeroom FROM cms_transactions
		WHERE bin_id = $1 AND id > $2 ORDER BY id`, bin, m.cms)
	mustNil(u.t, err, "cms rows")
	defer rows.Close()
	var out []string
	for rows.Next() {
		var part, room string
		var delta int64
		mustNil(u.t, rows.Scan(&part, &delta, &room), "cms row")
		out = append(out, fmt.Sprintf("%s%+d@%s", part, delta, room))
	}
	mustNil(u.t, rows.Err(), "cms rows")
	if len(out) == 0 {
		return "-"
	}
	return strings.Join(out, " ")
}

// ledgerOps is the ops Core's count ledger wrote for bin since m, in order
// (ppOps: the lifts dropped), "-" for none.
func (u *uopCell) ledgerOps(m ppMark, bin int64) string {
	u.t.Helper()
	rows, err := audit.ListBinUOPByBin(u.core.eng.DB().DB, bin, 500, 0)
	mustNil(u.t, err, "the bin's ledger")
	var ops []string
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].ID > m.ledger {
			ops = append(ops, rows[i].Op)
		}
	}
	return ppOps(ops)
}

// inStoreroom fails unless Core has bin in a slot of the storeroom.
func (u *uopCell) inStoreroom(what string, bin int64) {
	u.t.Helper()
	b, err := u.core.eng.DB().GetBin(bin)
	mustNil(u.t, err, what)
	if b.NodeID != nil {
		if n, err := u.core.eng.DB().GetNode(*b.NodeID); err == nil && n.ParentID != nil && u.nodeName(n.ParentID) == ksrDest {
			return
		}
	}
	u.dump(what)
	u.t.Fatalf("%s: bin %d stands at %s, want a slot of the storeroom %s", what, bin, u.nodeName(b.NodeID), ksrDest)
}

// freeStoreroomSlot is a slot of the storeroom no bin stands in.
func (u *uopCell) freeStoreroomSlot() *int64 {
	u.t.Helper()
	db := u.core.eng.DB()
	all, err := db.ListBins()
	mustNil(u.t, err, "bins")
	taken := map[int64]bool{}
	for _, b := range all {
		if b.NodeID != nil {
			taken[*b.NodeID] = true
		}
	}
	for i := 1; i <= 6; i++ {
		n, err := db.GetNodeByName(fmt.Sprintf("%s-%d", ksrDest, i))
		mustNil(u.t, err, "a storeroom slot")
		if !taken[n.ID] {
			return &n.ID
		}
	}
	u.t.Fatal("the storeroom has no free slot")
	return nil
}

// catalogBeat is one heartbeat from the Edge quoting the catalog digest it
// holds, carried to Core, whose answer carries the catalog when the digests
// differ (CoreDataService.answerFeeds); it waits until the Edge's row of code
// satisfies ok. A plant's heartbeat quotes every feed and Core answers each key
// on its own, so quoting the catalog alone leaves the catalog's answer as it is
// and asks for no feed this Edge is not routed for.
func (u *uopCell) catalogBeat(label, code string, ok func(*catalog.CatalogEntry) bool) {
	u.t.Helper()
	held := u.edge.Engine.FeedDigests()[protocol.FeedCatalog]
	env, err := protocol.NewDataEnvelope(protocol.SubjectEdgeHeartbeat,
		protocol.Address{Role: protocol.RoleEdge, Station: "edge.test"}, protocol.Address{Role: protocol.RoleCore},
		&protocol.EdgeHeartbeat{StationID: "edge.test", Feeds: map[string]string{protocol.FeedCatalog: held}})
	mustNil(u.t, err, label+": the heartbeat")
	raw, err := env.Encode()
	mustNil(u.t, err, label+": encode the heartbeat")
	_, err = u.edge.DB.EnqueueOutbox(raw, "data."+protocol.SubjectEdgeHeartbeat)
	mustNil(u.t, err, label+": send the heartbeat")
	u.eventually(label+": the catalog at the Edge", func() bool {
		e, err := u.edge.DB.GetPayloadCatalogByCode(code)
		return err == nil && ok(e)
	})
}

// consumeLine adds an assembly to the Edge: Core's nodes for its line and its
// stagings, and a process on a style of its own whose one consume claim draws
// part from the storeroom and sends its carriers back there. pack is the
// claim's capacity field (the claim reads the catalog's). It returns the
// process, its node and the style.
func (u *uopCell) consumeLine(line, part string, pack int) (proc, node, style int64) {
	u.t.Helper()
	t, edb := u.t, u.edge.DB
	for _, n := range []string{line, line + "-IN", line + "-OUT"} {
		mustNil(t, u.core.eng.DB().CreateNode(&corenodes.Node{Name: n, Enabled: true}), "node "+n)
	}
	proc, err := edb.CreateProcess(line+"-PROC", "an assembly", "", "", false)
	mustNil(t, err, line+": the process")
	node, err = edb.CreateProcessNode(processes.NodeInput{
		ProcessID: proc, CoreNodeName: line, Code: line, Name: line, Sequence: 1, Enabled: true,
	})
	mustNil(t, err, line+": the node")
	_, err = edb.EnsureProcessNodeRuntime(node)
	mustNil(t, err, line+": the runtime")
	style, err = edb.CreateStyle(line+"-S", "", proc)
	mustNil(t, err, line+": the style")
	mustNil(t, edb.SetActiveStyle(proc, &style), line+": the style active")
	claim, err := edb.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
		StyleID: style, CoreNodeName: line, Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeTwoRobot,
		PayloadCode: part, UOPCapacity: pack, ReorderPoint: 10,
		InboundSource: ksrDest, InboundStaging: line + "-IN", OutboundStaging: line + "-OUT",
		OutboundDestination: ksrDest, KeepStagedNode: domain.Ptr(""), AutoReorder: domain.Ptr(false),
	})
	mustNil(t, err, line+": the claim")
	mustNil(t, edb.SetProcessNodeRuntime(node, &claim, 0), line+": the seat")
	// The watch list is held by the process-wide node generation
	// (processes.NodeGeneration), which a claim does not move. A bins push
	// between the node's write and the claim's holds a list without the claim's
	// stagings, and a parallel test's node write would rebuild it under the
	// REQUEST, which then reads "built for another list" as occupied. So the
	// list is rebuilt here, and the line is returned once the Edge may decide
	// from a copy that answers its three places.
	edge := u.edge.Engine
	edge.StationService().InvalidateBinNodes()
	u.waitFor(line+": the bins copy answers the new line", 15*time.Second, func() bool {
		if ok, _ := edge.BinsActable(); !ok {
			return false
		}
		rows := edge.LocalNodeBins().Rows
		for _, n := range []string{line, line + "-IN", line + "-OUT"} {
			if _, ok := rows[n]; !ok {
				return false
			}
		}
		return true
	})
	return proc, node, style
}

// deliver is the operator's REQUEST at an empty assembly: one plain delivery,
// carried by the fleet and confirmed. It returns the bin the seat then holds.
func (u *uopCell) deliver(label string, node int64) int64 {
	u.t.Helper()
	res, err := u.edge.Engine.RequestNodeMaterial(node, 1)
	mustNil(u.t, err, label)
	if res == nil || res.Order == nil {
		u.dump(label)
		u.t.Fatalf("%s made no delivery: %+v", label, res)
	}
	d := *res.Order
	u.driveUntil(label+": delivered and confirmed", func() bool {
		u.fleetStep()
		if u.edgeRow(d.ID).Status == protocol.StatusDelivered {
			mustNil(u.t, u.edge.Engine.OrderManager().ConfirmDelivery(d.ID, 1), label+": confirm")
		}
		return protocol.IsTerminal(u.edgeRow(d.ID).Status) && protocol.IsTerminal(u.coreOf(d).Status)
	})
	if _, bin := u.seat(node); bin != 0 {
		return bin
	}
	u.dump(label)
	u.t.Fatalf("%s: the seat holds no bin after the delivery", label)
	return 0
}

// routeFirst has in hand the data subjects subjects routes to it, and every
// other envelope on to the dispatch it had.
func routeFirst(in *protocol.Ingestor, subjects *router.SubjectRouter) {
	inner := in.Dispatch
	in.Dispatch = func(env *protocol.Envelope) {
		if env.Type == protocol.TypeData {
			var d protocol.Data
			if err := json.Unmarshal(env.Payload, &d); err == nil && subjects.Has(d.Subject) {
				subjects.Dispatch(env, &d)
				return
			}
		}
		inner(env)
	}
}

// tickN is n strokes of 1 on a process's counter under its style, as the
// counter's delta reaches the engine.
func tickN(c *ksrCell, process, style int64, n int) {
	for i := 0; i < n; i++ {
		c.edge.Engine.Events.Emit(edgeengine.Event{Type: edgeengine.EventCounterDelta,
			Payload: edgeengine.CounterDeltaEvent{ProcessID: process, StyleID: style, Delta: 1}})
	}
}
