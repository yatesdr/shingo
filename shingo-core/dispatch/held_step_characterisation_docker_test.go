//go:build docker

package dispatch

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/domain"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// held_step_characterisation_docker_test.go — the three flows the held step
// (the vacate wait kind) must leave alone, recorded end to end: the swap return
// to the vacated slot (SPR 2026-09-28, pairs 7060/7061 and 7062/7063), the
// two-robot evac, and the press-index swap.
//
// Each case goes through the real intake and one pass, then presses RELEASE in
// the order the Edge's act sends it (lifters first) and records, per leg: the
// persisted plan (steps_json, bytes), the fleet create's blocks, what the
// staged notice tells the station (its wait ordinal and kind) and the cause on
// the row, and per press: the appends, the blocks appended, the error codes,
// wait_index, status and cause.
//
// steps_json is compared as bytes with three substitutions, all of values a
// fresh run cannot reproduce: a stamp's bin becomes the bin's label, its
// partner the partner's uuid, and its stamped_at / held_at (the vacate kind's
// times, Core's clock) the marker <T>. Nothing else is normalised: a stamp
// gaining or losing a field is a diff.

// ── harness ─────────────────────────────────────────────────────────────────

type htRig struct {
	t       *testing.T
	db      *store.DB
	d       *Dispatcher
	backend *testdb.MockTrackingBackend
	labels  map[int64]string // bin id → label
	uuids   map[int64]string // order id → edge uuid
}

func newHTRig(t *testing.T, db *store.DB, d *Dispatcher, backend *testdb.MockTrackingBackend) *htRig {
	return &htRig{t: t, db: db, d: d, backend: backend, labels: map[int64]string{}, uuids: map[int64]string{}}
}

func (r *htRig) bin(id int64, label string) { r.labels[id] = label }

// htWait is the station wait every Edge builder authors (stationWait in
// shingo-edge/engine/material_orders.go): it carries its owner.
func htWait(n string) protocol.ComplexOrderStep {
	return protocol.ComplexOrderStep{Action: protocol.ActionWait, Node: n, WaitKind: protocol.WaitKindStation}
}

var (
	htIDs   = regexp.MustCompile(`"(bin|partner)":(\d+)`)
	htTimes = regexp.MustCompile(`"(stamped_at|held_at)":"[^"]+"`)
)

// steps renders an order's persisted steps_json with stamp ids named, and a
// stamp's times (the vacate kind's stamped_at and held_at, Core's clock) as
// <T>: present or absent is pinned, the instant is not.
func (r *htRig) steps(uuid string) string {
	r.t.Helper()
	o := prReloadUUID(r.t, r.db, uuid)
	raw := htTimes.ReplaceAllString(o.StepsJSON, `"$1":"<T>"`)
	return htIDs.ReplaceAllStringFunc(raw, func(m string) string {
		sub := htIDs.FindStringSubmatch(m)
		var id int64
		fmt.Sscan(sub[2], &id)
		if sub[1] == "bin" {
			if l, ok := r.labels[id]; ok {
				return `"bin":"` + l + `"`
			}
			return m
		}
		if u, ok := r.uuids[id]; ok {
			return `"partner":"` + u + `"`
		}
		return m
	})
}

func htBlocks(blocks []fleetBlock) string {
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		parts = append(parts, b.Location+"/"+b.BinTask)
	}
	return strings.Join(parts, " ")
}

type fleetBlock struct{ Location, BinTask string }

// create is the fleet create's blocks for one leg (the SplitAtWait prefix).
func (r *htRig) create(uuid string) string {
	r.t.Helper()
	for _, c := range r.backend.CreateRequests() {
		if c.ExternalID == uuid {
			bs := make([]fleetBlock, 0, len(c.Blocks))
			for _, b := range c.Blocks {
				bs = append(bs, fleetBlock{b.Location, b.BinTask})
			}
			return htBlocks(bs)
		}
	}
	return "<no create>"
}

// stage puts a dispatched leg at its first wait the way the fleet poll does
// (status staged, then MarkStationWaitIfOwned) and returns what the staged
// notice tells the station.
func (r *htRig) stage(uuid string) string {
	r.t.Helper()
	o := prReloadUUID(r.t, r.db, uuid)
	r.uuids[o.ID] = uuid
	testdb.SeedOrderStatus(r.t, r.db, o.ID, string(StatusStaged), "")
	at, kind := r.d.MarkStationWaitIfOwned(o.ID)
	ord := "nil"
	if at != nil {
		ord = fmt.Sprint(*at)
	}
	return fmt.Sprintf("wait=%s kind=%s cause=%s", ord, kind, prReloadUUID(r.t, r.db, uuid).QueueCause)
}

// press is one RELEASE as an echoing Edge sends it, and what it did.
func (r *htRig) press(uuid string, echo int) string {
	r.t.Helper()
	before, errs := len(r.backend.ReleaseCalls()), len(orderErrorCodes(r.t, r.db, uuid))
	r.d.HandleOrderRelease(r.d.syntheticEnvelope("line-1"),
		&protocol.OrderRelease{OrderUUID: uuid, StationWait: &echo})
	var appended []string
	for _, c := range r.backend.ReleaseCalls()[before:] {
		bs := make([]fleetBlock, 0, len(c.Blocks))
		for _, b := range c.Blocks {
			bs = append(bs, fleetBlock{b.Location, b.BinTask})
		}
		appended = append(appended, htBlocks(bs))
	}
	o := prReloadUUID(r.t, r.db, uuid)
	return fmt.Sprintf("%s: appends=[%s] errors=[%s] wait_index=%d status=%s cause=%s",
		uuid, strings.Join(appended, "; "), strings.Join(orderErrorCodes(r.t, r.db, uuid)[errs:], ","),
		o.WaitIndex, o.Status, o.QueueCause)
}

// lift is the robot's pickup as Core records it: the store's lift, the one
// statement the engine's BinService.Lift runs.
func (r *htRig) lift(binID int64) {
	r.t.Helper()
	transit, err := r.db.GetNodeByName(domain.TransitNodeName)
	testutil.MustNoErr(r.t, err, "transit node")
	tx, err := r.db.Begin()
	testutil.MustNoErr(r.t, err, "begin lift")
	defer func(tx *sql.Tx) { _ = tx.Rollback() }(tx)
	_, err = r.db.LiftBinToTransitTx(tx, binID, transit.ID, "", time.Now().UTC())
	testutil.MustNoErr(r.t, err, "lift")
	testutil.MustNoErr(r.t, tx.Commit(), "commit lift")
}

// notice is what the next staged notice tells the station about a leg, read
// without moving it (MarkStationWaitIfOwned, as the fleet poll calls it).
func (r *htRig) notice(uuid string) string {
	r.t.Helper()
	at, kind := r.d.MarkStationWaitIfOwned(prReloadUUID(r.t, r.db, uuid).ID)
	ord := "nil"
	if at != nil {
		ord = fmt.Sprint(*at)
	}
	return fmt.Sprintf("wait=%s kind=%s cause=%s", ord, kind, prReloadUUID(r.t, r.db, uuid).QueueCause)
}

// liftAndRedrive is the lift and the event it fires: the engine re-drives the
// vacate holds that name the bin (RedriveVacateWaits, wiring_lane_gate.go). It
// returns what was appended, by order, in the order appended.
func (r *htRig) liftAndRedrive(binID int64) string {
	r.t.Helper()
	r.lift(binID)
	before := len(r.backend.ReleaseCalls())
	r.d.RedriveVacateWaits(binID)
	byVendor := map[string]string{}
	for _, u := range r.uuids {
		byVendor[prReloadUUID(r.t, r.db, u).VendorOrderID] = u
	}
	var out []string
	for _, c := range r.backend.ReleaseCalls()[before:] {
		bs := make([]fleetBlock, 0, len(c.Blocks))
		for _, b := range c.Blocks {
			bs = append(bs, fleetBlock{b.Location, b.BinTask})
		}
		out = append(out, byVendor[c.VendorOrderID]+": "+htBlocks(bs))
	}
	return "appended=[" + strings.Join(out, "; ") + "]"
}

func htEq(t *testing.T, what, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s:\n got: %s\nwant: %s", what, got, want)
	}
}

// ── C1: the swap return to the vacated slot ─────────────────────────────────

// htAppended is a press that appended its segment.
func htAppended(uuid, blocks string) string {
	return uuid + ": appends=[" + blocks + "] errors=[] wait_index=1 status=in_transit cause="
}

const htStagedAtStation = "wait=0 kind=station cause=station-wait"

// htSwapReturn builds the SPR 2026-09-28 pair after its one pass: the supply
// widened FIFO onto the buffer partial, the return granted the buffer its
// supply empties (grant (b), stamped).
func htSwapReturn(t *testing.T, prefix string) (r *htRig, supply, ret string, partialID, residentID int64) {
	t.Helper()
	db := testDB(t)
	home, buffer, _, _ := parkFixture(t, db)
	backend := testdb.NewTrackingBackend()
	d, _ := newTestDispatcher(t, db, backend)
	r = newHTRig(t, db, d, backend)
	line, stage := prNode(t, db, prefix+"-LINE"), prNode(t, db, prefix+"-STAGE")
	now := time.Now().UTC()
	full := makeLoaderBin(t, db, "PART-X", home.ID, prefix+"-HOME-FULL", 10, now)
	partial := makeLoaderBin(t, db, "PART-X", buffer.ID, prefix+"-PARTIAL", 4, now.Add(-2*time.Hour))
	resident := makeLoaderBin(t, db, "PART-X", line.ID, prefix+"-RESIDENT", 3, now)
	r.bin(full.ID, prefix+"-HOME-FULL")
	r.bin(partial.ID, prefix+"-PARTIAL")
	r.bin(resident.ID, prefix+"-RESIDENT")
	supply, ret = strings.ToLower(prefix)+"-supply", strings.ToLower(prefix)+"-return"
	prSubmitLeg(d, supply, ret, "PART-X", line.Name,
		prPick(home.Name), prDropExcl(stage.Name), htWait(stage.Name), prPick(stage.Name), prDrop(line.Name))
	prSubmitLeg(d, ret, supply, "PART-X", line.Name,
		htWait(line.Name), prPick(line.Name), prDrop(home.Name))
	prScanPass(t, d, db, ret, supply)
	for _, u := range []string{supply, ret} {
		if o := prReloadUUID(t, db, u); o.VendorOrderID == "" {
			t.Fatalf("fixture: %s did not dispatch (status %s cause %q)", u, o.Status, o.QueueCause)
		}
	}
	r.uuids[prReloadUUID(t, db, supply).ID] = supply
	r.uuids[prReloadUUID(t, db, ret).ID] = ret
	htSwapReturnPlans(t, r, supply, ret, prefix)
	htEq(t, "return staged", r.stage(ret), htStagedAtStation)
	htEq(t, "supply staged", r.stage(supply), htStagedAtStation)
	return r, supply, ret, partial.ID, resident.ID
}

// htSwapReturnPlans: the supply lifts the buffer partial (widened off the home,
// LX-P1, onto LX-B1); the return drops on LX-B1, stamped on that partial and
// that partner, anchored at the home it was authored at.
func htSwapReturnPlans(t *testing.T, r *htRig, supply, ret, p string) {
	t.Helper()
	htEq(t, "supply steps_json", r.steps(supply),
		`[{"action":"pickup","node":"LX-B1","group":"LX-P1"},{"action":"dropoff","node":"`+p+`-STAGE","exclusive_slot":true},`+
			`{"action":"wait","node":"`+p+`-STAGE","wait_kind":"station"},{"action":"pickup","node":"`+p+`-STAGE"},`+
			`{"action":"dropoff","node":"`+p+`-LINE"}]`)
	htEq(t, "return steps_json", r.steps(ret),
		`[{"action":"wait","node":"`+p+`-LINE","wait_kind":"station"},{"action":"pickup","node":"`+p+`-LINE"},`+
			`{"action":"dropoff","node":"LX-B1","vacate":{"node":"LX-B1","bin":"`+p+`-PARTIAL","partner":"`+supply+`","stamped_at":"<T>"},"anchor":"LX-P1"}]`)
	htEq(t, "supply create", r.create(supply), "LX-B1/JackLoad "+p+"-STAGE/JackUnload "+p+"-STAGE/Wait")
	htEq(t, "return create", r.create(ret), p+"-LINE/Wait")
}

// TestHeldStepBase_SwapReturnToTheVacatedSlot — C1. Three orders of events
// around the supply's lift of the buffer partial.
func TestHeldStepBase_SwapReturnToTheVacatedSlot(t *testing.T) {
	t.Parallel()
	t.Run("a: lifted before the click", func(t *testing.T) {
		t.Parallel()
		r, supply, ret, partial, _ := htSwapReturn(t, "HTA")
		r.lift(partial)
		htEq(t, "press return", r.press(ret, 0), htAppended(ret, "HTA-LINE/JackLoad LX-B1/JackUnload"))
		htEq(t, "press supply", r.press(supply, 0), htAppended(supply, "HTA-STAGE/JackLoad HTA-LINE/JackUnload"))
	})
	// (b) and (c) are the RULED change (2026-10-18, the vacate kind). At the base
	// each press was refused (invalid_state, cause station-wait) and pressed
	// again after the lift. Now each press is taken and held (no error, cause
	// vacate-waiting-lift, the station told kind "vacate"), and Core appends the
	// SAME blocks in the SAME order — the return, then the supply — on the lifts:
	// the partial off the buffer releases the return; the return's lift of the
	// line's bin releases the supply (Y's stamp, written at its press).
	t.Run("b: not yet lifted at the click", func(t *testing.T) {
		t.Parallel()
		r, supply, ret, partial, resident := htSwapReturn(t, "HTB")
		htEq(t, "press return", r.press(ret, 0), htHeld(ret))
		htEq(t, "press supply", r.press(supply, 0), htHeld(supply))
		htEq(t, "return notice", r.notice(ret), htHeldNotice)
		htEq(t, "supply notice", r.notice(supply), htHeldNotice)
		htEq(t, "press return again (held)", r.press(ret, 0), htHeld(ret))
		htSwapReturnHeldPlans(t, r, supply, ret, "HTB")
		htEq(t, "lift the partial", r.liftAndRedrive(partial),
			"appended=["+ret+": HTB-LINE/JackLoad LX-B1/JackUnload]")
		htEq(t, "lift the line's bin", r.liftAndRedrive(resident),
			"appended=["+supply+": HTB-STAGE/JackLoad HTB-LINE/JackUnload]")
	})
	t.Run("c: supply's release first (outbox inversion)", func(t *testing.T) {
		t.Parallel()
		r, supply, ret, partial, resident := htSwapReturn(t, "HTC")
		htEq(t, "press supply", r.press(supply, 0), htHeld(supply))
		htEq(t, "press return", r.press(ret, 0), htHeld(ret))
		htSwapReturnHeldPlans(t, r, supply, ret, "HTC")
		htEq(t, "lift the partial", r.liftAndRedrive(partial),
			"appended=["+ret+": HTC-LINE/JackLoad LX-B1/JackUnload]")
		htEq(t, "lift the line's bin", r.liftAndRedrive(resident),
			"appended=["+supply+": HTC-STAGE/JackLoad HTC-LINE/JackUnload]")
	})
}

// htHeld is a press Core took and holds on a vacate stamp.
func htHeld(uuid string) string {
	return uuid + ": appends=[] errors=[] wait_index=0 status=staged cause=vacate-waiting-lift"
}

const htHeldNotice = "wait=nil kind=vacate cause=vacate-waiting-lift"

// htSwapReturnHeldPlans: holding adds fields to stamps and nothing else. The
// return's stamp gains held_at; the supply's line drop gains the sibling stamp
// on the line's bin, held. Steps, nodes and wait kinds are the base's.
func htSwapReturnHeldPlans(t *testing.T, r *htRig, supply, ret, p string) {
	t.Helper()
	htEq(t, "held supply steps_json", r.steps(supply),
		`[{"action":"pickup","node":"LX-B1","group":"LX-P1"},{"action":"dropoff","node":"`+p+`-STAGE","exclusive_slot":true},`+
			`{"action":"wait","node":"`+p+`-STAGE","wait_kind":"station"},{"action":"pickup","node":"`+p+`-STAGE"},`+
			`{"action":"dropoff","node":"`+p+`-LINE","vacate":{"node":"`+p+`-LINE","bin":"`+p+`-RESIDENT","partner":"`+ret+`",`+
			`"kind":"sibling","stamped_at":"<T>","held_at":"<T>"}}]`)
	htEq(t, "held return steps_json", r.steps(ret),
		`[{"action":"wait","node":"`+p+`-LINE","wait_kind":"station"},{"action":"pickup","node":"`+p+`-LINE"},`+
			`{"action":"dropoff","node":"LX-B1","vacate":{"node":"LX-B1","bin":"`+p+`-PARTIAL","partner":"`+supply+`",`+
			`"stamped_at":"<T>","held_at":"<T>"},"anchor":"LX-P1"}]`)
}

// ── C2: the two-robot evac ──────────────────────────────────────────────────

// TestHeldStepBase_TwoRobotEvac — C2. (a) the ordinary pair, nothing stamped;
// (b) the evac into its own full market, its drop granted the child its supply
// empties.
func TestHeldStepBase_TwoRobotEvac(t *testing.T) {
	t.Parallel()
	t.Run("a: ordinary", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		sd := testdb.SetupStandardData(t, db)
		backend := testdb.NewTrackingBackend()
		d, _ := newTestDispatcher(t, db, backend)
		r := newHTRig(t, db, d, backend)
		stage, out := prNode(t, db, "HTE-STAGE"), prNode(t, db, "HTE-OUT")
		fresh := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "HTE-FRESH")
		res := prResident(t, db, sd.LineNode, sd.BinType.ID, sd.Payload.Code, "HTE-RESIDENT")
		r.bin(fresh.ID, "HTE-FRESH")
		r.bin(res.ID, "HTE-RESIDENT")
		prSubmitLeg(d, "hte-supply", "hte-evac", sd.Payload.Code, sd.LineNode.Name,
			prPick(sd.StorageNode.Name), prDropExcl(stage.Name), htWait(stage.Name), prPick(stage.Name),
			prDrop(sd.LineNode.Name))
		prSubmitLeg(d, "hte-evac", "hte-supply", sd.Payload.Code, sd.LineNode.Name,
			htWait(sd.LineNode.Name), prPick(sd.LineNode.Name), prDrop(out.Name))
		prScanPass(t, d, db, "hte-evac", "hte-supply")
		htEq(t, "supply steps_json", r.steps("hte-supply"),
			`[{"action":"pickup","node":"STORAGE-A1"},{"action":"dropoff","node":"HTE-STAGE","exclusive_slot":true},`+
				`{"action":"wait","node":"HTE-STAGE","wait_kind":"station"},{"action":"pickup","node":"HTE-STAGE"},`+
				`{"action":"dropoff","node":"LINE1-IN"}]`)
		htEq(t, "evac steps_json", r.steps("hte-evac"),
			`[{"action":"wait","node":"LINE1-IN","wait_kind":"station"},{"action":"pickup","node":"LINE1-IN"},`+
				`{"action":"dropoff","node":"HTE-OUT"}]`)
		htEq(t, "supply create", r.create("hte-supply"), "STORAGE-A1/JackLoad HTE-STAGE/JackUnload HTE-STAGE/Wait")
		htEq(t, "evac create", r.create("hte-evac"), "LINE1-IN/Wait")
		htEq(t, "evac staged", r.stage("hte-evac"), htStagedAtStation)
		htEq(t, "supply staged", r.stage("hte-supply"), htStagedAtStation)
		r.lift(fresh.ID)
		htEq(t, "press evac", r.press("hte-evac", 0), htAppended("hte-evac", "LINE1-IN/JackLoad HTE-OUT/JackUnload"))
		htEq(t, "press supply", r.press("hte-supply", 0),
			htAppended("hte-supply", "HTE-STAGE/JackLoad LINE1-IN/JackUnload"))
	})
	t.Run("b: into its own full market", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		sd := testdb.SetupStandardData(t, db)
		d, backend := newTestDispatcherWithResolver(t, db)
		r := newHTRig(t, db, d, backend)
		grp := vsGroup(t, db, "HTM-MKT")
		c1, c2 := vsChildOf(t, db, grp, "HTM-C1"), vsChildOf(t, db, grp, "HTM-C2")
		stage := prNode(t, db, "HTM-STAGE")
		fresh := testdb.CreateBinAtNode(t, db, sd.Payload.Code, c1.ID, "HTM-FRESH")
		other := makeEmptyBin(t, db, c2.ID, "HTM-OTHER")
		res := prResident(t, db, sd.LineNode, sd.BinType.ID, sd.Payload.Code, "HTM-RESIDENT")
		r.bin(fresh.ID, "HTM-FRESH")
		r.bin(other.ID, "HTM-OTHER")
		r.bin(res.ID, "HTM-RESIDENT")
		prSubmitLeg(d, "htm-supply", "htm-evac", sd.Payload.Code, sd.LineNode.Name,
			prPick(grp.Name), prDropExcl(stage.Name), htWait(stage.Name), prPick(stage.Name), prDrop(sd.LineNode.Name))
		prSubmitLeg(d, "htm-evac", "htm-supply", sd.Payload.Code, sd.LineNode.Name,
			htWait(sd.LineNode.Name), prPick(sd.LineNode.Name), prDrop(grp.Name))
		prScanPass(t, d, db, "htm-evac", "htm-supply")
		r.uuids[prReloadUUID(t, db, "htm-supply").ID] = "htm-supply"
		r.uuids[prReloadUUID(t, db, "htm-evac").ID] = "htm-evac"
		htEq(t, "supply steps_json", r.steps("htm-supply"),
			`[{"action":"pickup","node":"HTM-C1","group":"HTM-MKT"},{"action":"dropoff","node":"HTM-STAGE","exclusive_slot":true},`+
				`{"action":"wait","node":"HTM-STAGE","wait_kind":"station"},{"action":"pickup","node":"HTM-STAGE"},`+
				`{"action":"dropoff","node":"LINE1-IN"}]`)
		htEq(t, "evac steps_json", r.steps("htm-evac"),
			`[{"action":"wait","node":"LINE1-IN","wait_kind":"station"},{"action":"pickup","node":"LINE1-IN"},`+
				`{"action":"dropoff","node":"HTM-C1","group":"HTM-MKT","vacate":{"node":"HTM-C1","bin":"HTM-FRESH","partner":"htm-supply","stamped_at":"<T>"}}]`)
		htEq(t, "supply create", r.create("htm-supply"), "HTM-C1/JackLoad HTM-STAGE/JackUnload HTM-STAGE/Wait")
		htEq(t, "evac create", r.create("htm-evac"), "LINE1-IN/Wait")
		htEq(t, "evac staged", r.stage("htm-evac"), htStagedAtStation)
		htEq(t, "supply staged", r.stage("htm-supply"), htStagedAtStation)
		r.lift(fresh.ID)
		htEq(t, "press evac", r.press("htm-evac", 0), htAppended("htm-evac", "LINE1-IN/JackLoad HTM-C1/JackUnload"))
		htEq(t, "press supply", r.press("htm-supply", 0),
			htAppended("htm-supply", "HTM-STAGE/JackLoad LINE1-IN/JackUnload"))
	})
}

// ── C3: the press-index swap ────────────────────────────────────────────────

// TestHeldStepBase_PressIndexReleaseOrder — C3. 2- and 3-position, unflipped
// and flipped (IndexRobotSupplies), the shapes BuildTwoRobotPressIndexSwapSteps
// authors; R1 then R2, and R2 then R1. No grant applies to any of them, so no
// step carries a stamp, and R2 dropping on the front R1 has not lifted is not
// fenced.
func TestHeldStepBase_PressIndexReleaseOrder(t *testing.T) {
	t.Parallel()
	const (
		r1Tail2 = `,{"action":"pickup","node":"HTP-IN"},{"action":"dropoff","node":"HTP-B"}]`
		r1Tail3 = `,{"action":"pickup","node":"HTP-IN"},{"action":"dropoff","node":"HTP-C"}]`
		r1Head  = `[{"action":"wait","node":"LINE1-IN","wait_kind":"station"},{"action":"pickup","node":"LINE1-IN"},{"action":"dropoff","node":"HTP-OUT"}`
		r2Head  = `[{"action":"wait","node":"HTP-B","wait_kind":"station"},{"action":"pickup","node":"HTP-B"},{"action":"dropoff","node":"LINE1-IN"}`
		r2Shift = `,{"action":"pickup","node":"HTP-C"},{"action":"dropoff","node":"HTP-B"}`
	)
	for _, tc := range []struct {
		name           string
		threePos, flip bool
		r2First        bool
		wantR1, wantR2 string
		blkR1, blkR2   string
	}{
		{name: "2-pos R1 first",
			wantR1: r1Head + r1Tail2, wantR2: r2Head + "]",
			blkR1: "LINE1-IN/JackLoad HTP-OUT/JackUnload HTP-IN/JackLoad HTP-B/JackUnload",
			blkR2: "HTP-B/JackLoad LINE1-IN/JackUnload"},
		{name: "2-pos R2 first", r2First: true,
			wantR1: r1Head + r1Tail2, wantR2: r2Head + "]",
			blkR1: "LINE1-IN/JackLoad HTP-OUT/JackUnload HTP-IN/JackLoad HTP-B/JackUnload",
			blkR2: "HTP-B/JackLoad LINE1-IN/JackUnload"},
		{name: "3-pos R1 first", threePos: true,
			wantR1: r1Head + r1Tail3, wantR2: r2Head + r2Shift + "]",
			blkR1: "LINE1-IN/JackLoad HTP-OUT/JackUnload HTP-IN/JackLoad HTP-C/JackUnload",
			blkR2: "HTP-B/JackLoad LINE1-IN/JackUnload HTP-C/JackLoad HTP-B/JackUnload"},
		{name: "2-pos flipped R1 first", flip: true,
			wantR1: r1Head + "]", wantR2: r2Head + r1Tail2,
			blkR1: "LINE1-IN/JackLoad HTP-OUT/JackUnload",
			blkR2: "HTP-B/JackLoad LINE1-IN/JackUnload HTP-IN/JackLoad HTP-B/JackUnload"},
		{name: "3-pos flipped R1 first", threePos: true, flip: true,
			wantR1: r1Head + "]", wantR2: r2Head + r2Shift + r1Tail3,
			blkR1: "LINE1-IN/JackLoad HTP-OUT/JackUnload",
			blkR2: "HTP-B/JackLoad LINE1-IN/JackUnload HTP-C/JackLoad HTP-B/JackUnload HTP-IN/JackLoad HTP-C/JackUnload"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			sd := testdb.SetupStandardData(t, db)
			backend := testdb.NewTrackingBackend()
			d, _ := newTestDispatcher(t, db, backend)
			r := newHTRig(t, db, d, backend)
			p := "HTP"
			front := sd.LineNode
			b := prNode(t, db, p+"-B")
			out, inb := prNode(t, db, p+"-OUT"), prNode(t, db, p+"-IN")
			r.bin(prResident(t, db, front, sd.BinType.ID, sd.Payload.Code, p+"-FRONT").ID, p+"-FRONT")
			r.bin(prResident(t, db, b, sd.BinType.ID, sd.Payload.Code, p+"-ONDECK").ID, p+"-ONDECK")
			back := b
			var c string
			if tc.threePos {
				cn := prNode(t, db, p+"-C")
				c, back = cn.Name, cn
				r.bin(prResident(t, db, cn, sd.BinType.ID, sd.Payload.Code, p+"-THIRD").ID, p+"-THIRD")
			}
			r.bin(testdb.CreateBinAtNode(t, db, sd.Payload.Code, inb.ID, p+"-FRESH").ID, p+"-FRESH")
			r1 := []protocol.ComplexOrderStep{htWait(front.Name), prPick(front.Name), prDrop(out.Name)}
			r2 := []protocol.ComplexOrderStep{htWait(b.Name), prPick(b.Name), prDrop(front.Name)}
			if tc.threePos {
				r2 = append(r2, prPick(c), prDrop(b.Name))
			}
			if tc.flip {
				r2 = append(r2, prPick(inb.Name), prDrop(back.Name))
			} else {
				r1 = append(r1, prPick(inb.Name), prDrop(back.Name))
			}
			prSubmitLeg(d, "htp-r1", "htp-r2", sd.Payload.Code, front.Name, r1...)
			prSubmitLeg(d, "htp-r2", "htp-r1", sd.Payload.Code, front.Name, r2...)
			prScanPass(t, d, db, "htp-r2", "htp-r1")
			for _, u := range []string{"htp-r1", "htp-r2"} {
				if o := prReloadUUID(t, db, u); o.VendorOrderID == "" {
					t.Fatalf("fixture: %s did not dispatch (status %s cause %q)", u, o.Status, o.QueueCause)
				}
			}
			htEq(t, "R1 steps_json", r.steps("htp-r1"), tc.wantR1)
			htEq(t, "R2 steps_json", r.steps("htp-r2"), tc.wantR2)
			htEq(t, "R1 create", r.create("htp-r1"), "LINE1-IN/Wait")
			htEq(t, "R2 create", r.create("htp-r2"), "HTP-B/Wait")
			htEq(t, "R1 staged", r.stage("htp-r1"), htStagedAtStation)
			htEq(t, "R2 staged", r.stage("htp-r2"), htStagedAtStation)
			first, second, blk1, blk2 := "htp-r1", "htp-r2", tc.blkR1, tc.blkR2
			if tc.r2First {
				first, second, blk1, blk2 = second, first, blk2, blk1
			}
			htEq(t, "press first", r.press(first, 0), htAppended(first, blk1))
			htEq(t, "press second", r.press(second, 0), htAppended(second, blk2))
		})
	}
}
