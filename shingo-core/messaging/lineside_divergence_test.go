//go:build docker

package messaging

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/nodes"
)

// lineside_divergence_test.go — the lineside report is a per-carrier checksum,
// compared on ingest (seat-count round 1 §5 and S2, round 2 S8).
//
// Each report row names the carrier the Edge has bound at a seat (bin id,
// epoch), its count, and the highest delta seq the Edge had handed its outbox
// for that carrier generation. Core compares the row against its own replica
// and records a disagreement as a report_divergence episode in
// bin_uop_exception: opened when first seen, recovered when a later report
// agrees. The Edge states each count as of its flushed_seq, so a count gap is
// a divergence only when Core has applied exactly that far (Core's last_seq ==
// the row's flushed_seq); short of it is a delta in flight.
//
// The reports are built from JSON on purpose: it is what crosses the wire, and
// an Edge that predates the carrier keys sends the same shape without them.

// divergenceRig is one station, one consume seat Core knows about through the
// plant-claims mirror, and the handler under test.
type divergenceRig struct {
	t       *testing.T
	db      *store.DB
	svc     *CoreDataService
	station string
	seat    *nodes.Node
	at      time.Time
}

func newDivergenceRig(t *testing.T, tag string) *divergenceRig {
	t.Helper()
	db := testdb.Open(t)
	svc := newLinesideService(t, db)
	seat := &nodes.Node{Name: "ALN_DV_" + tag, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(seat), "create seat")
	// The plant-claims mirror says this seat is a consume seat of an active style.
	_, err := db.Exec(`INSERT INTO process_styles (process_id, style_id, config_gen, is_active)
		VALUES ($1, 'S1', 1, true)`, "PROC-DV-"+tag)
	testutil.MustNoErr(t, err, "seed active style")
	_, err = db.Exec(`INSERT INTO style_claims (process_id, style_id, core_node_name, role, swap_mode, payload_code)
		VALUES ($1, 'S1', $2, 'consume', 'manual_swap', 'PART-A')`, "PROC-DV-"+tag, seat.Name)
	testutil.MustNoErr(t, err, "seed consume claim")
	return &divergenceRig{
		t: t, db: db, svc: svc, station: "stn-dv-" + tag, seat: seat,
		at: time.Now().UTC().Truncate(time.Millisecond),
	}
}

// carrier puts a Core bin of payload at node with uop and returns its id and epoch.
func (r *divergenceRig) carrier(nodeID int64, label, payload string, uop int) (int64, int64) {
	r.t.Helper()
	b := testdb.CreateBinAtNode(r.t, r.db, payload, nodeID, label)
	_, err := r.db.Exec(`UPDATE bins SET uop_remaining=$1, status='staged' WHERE id=$2`, uop, b.ID)
	testutil.MustNoErr(r.t, err, "set carrier uop")
	var epoch int64
	testutil.MustNoErr(r.t, r.db.QueryRow(`SELECT delta_epoch FROM bins WHERE id=$1`, b.ID).Scan(&epoch), "read epoch")
	return b.ID, epoch
}

// lastSeq plants Core's dedup high-water mark for (station, bin, epoch).
func (r *divergenceRig) lastSeq(binID, epoch, seq int64) {
	r.t.Helper()
	_, err := r.db.Exec(`INSERT INTO inventory_delta_dedup (station, scope_kind, scope_key, epoch, last_seq)
		VALUES ($1, 'bin', $2, $3, $4)
		ON CONFLICT (station, scope_kind, scope_key, epoch) DO UPDATE SET last_seq = EXCLUDED.last_seq`,
		r.station, fmt.Sprint(binID), epoch, seq)
	testutil.MustNoErr(r.t, err, "plant dedup row")
}

// report delivers one report whose entries are given as JSON objects, stamped
// one minute after the previous report.
func (r *divergenceRig) report(entries ...string) {
	r.t.Helper()
	r.at = r.at.Add(time.Minute)
	body := fmt.Sprintf(`{"station":%q,"reported_at":%q,"entries":[`, r.station, r.at.Format(time.RFC3339Nano))
	for i, e := range entries {
		if i > 0 {
			body += ","
		}
		body += e
	}
	body += "]}"
	var rep protocol.LinesideLevelReport
	testutil.MustNoErr(r.t, json.Unmarshal([]byte(body), &rep), "decode report")
	r.svc.HandleLinesideLevelReport(linesideEnvelope(r.station), &rep)
}

// redeliver hands the handler the last report again, unchanged.
func (r *divergenceRig) redeliver(entries ...string) {
	r.t.Helper()
	r.at = r.at.Add(-time.Minute)
	r.report(entries...)
}

type openDivergence struct {
	class     string
	binID     sql.NullInt64
	node      string
	payload   string
	edgeCount sql.NullInt64
	coreCount sql.NullInt64
}

// open lists this station's open report_divergence episodes.
func (r *divergenceRig) open() []openDivergence {
	r.t.Helper()
	rows, err := r.db.Query(`SELECT op, bin_id, COALESCE(detail->>'node', ''), payload_code,
		       (detail->>'edge_count')::bigint, (detail->>'core_count')::bigint
		FROM bin_uop_exception
		WHERE kind = 'report_divergence' AND actor = $1 AND recovered_at IS NULL
		ORDER BY id`, r.station)
	testutil.MustNoErr(r.t, err, "list open divergences")
	defer rows.Close()
	var out []openDivergence
	for rows.Next() {
		var d openDivergence
		testutil.MustNoErr(r.t, rows.Scan(&d.class, &d.binID, &d.node, &d.payload, &d.edgeCount, &d.coreCount), "scan")
		out = append(out, d)
	}
	testutil.MustNoErr(r.t, rows.Err(), "rows")
	return out
}

// closed counts this station's recovered report_divergence episodes.
func (r *divergenceRig) closed() int {
	r.t.Helper()
	var n int
	testutil.MustNoErr(r.t, r.db.QueryRow(`SELECT count(*) FROM bin_uop_exception
		WHERE kind = 'report_divergence' AND actor = $1 AND recovered_at IS NOT NULL`, r.station).Scan(&n), "count closed")
	return n
}

func (r *divergenceRig) wantOne(class string, binID int64, edge, core int64) openDivergence {
	r.t.Helper()
	open := r.open()
	if len(open) != 1 {
		r.t.Fatalf("%d open report_divergence episode(s), want 1 (%s): %+v", len(open), class, open)
	}
	d := open[0]
	if d.class != class {
		r.t.Errorf("class = %q, want %q", d.class, class)
	}
	if binID != 0 && (!d.binID.Valid || d.binID.Int64 != binID) {
		r.t.Errorf("bin_id = %v, want %d", d.binID, binID)
	}
	if edge >= 0 && (!d.edgeCount.Valid || d.edgeCount.Int64 != edge) {
		r.t.Errorf("edge_count = %v, want %d", d.edgeCount, edge)
	}
	if core >= 0 && (!d.coreCount.Valid || d.coreCount.Int64 != core) {
		r.t.Errorf("core_count = %v, want %d", d.coreCount, core)
	}
	return d
}

func row(node, payload string, binID, epoch, flushed int64, binUOP, bucket int) string {
	return fmt.Sprintf(`{"core_node_name":%q,"payload_code":%q,"bin_count":1,"bin_uop":%d,"bucket_qty":%d,"bin_id":%d,"bin_epoch":%d,"flushed_seq":%d}`,
		node, payload, binUOP, bucket, binID, epoch, flushed)
}

// A SETTLED COUNT GAP OPENS ONE EPISODE, and it stays one across reports.
// Core holds 150; the Edge's bound carrier reads 10 with nothing flushed that
// Core has not applied. Verify-red at the base: the report is stored and
// nothing is compared, so nothing opens.
func TestLinesideDivergence_SettledCountGapOpensOneEpisode(t *testing.T) {
	t.Parallel()
	r := newDivergenceRig(t, "COUNT")
	bin, epoch := r.carrier(r.seat.ID, "BIN-DV-COUNT", "PART-A", 150)
	r.lastSeq(bin, epoch, 4)

	r.report(row(r.seat.Name, "PART-A", bin, epoch, 4, 10, 0))
	r.wantOne("count", bin, 10, 150)

	r.report(row(r.seat.Name, "PART-A", bin, epoch, 4, 9, 0))
	r.wantOne("count", bin, 10, 150)
}

// A GAP WITH DELTAS STILL IN FLIGHT IS NOT A DIVERGENCE. The Edge has flushed
// through seq 6; Core has applied through 5. The difference is the delta on the
// way, not a disagreement. Green at the base (nothing is compared there); after
// the change it is the rule that keeps the alarm quiet on a running line.
func TestLinesideDivergence_InFlightGapOpensNothing(t *testing.T) {
	t.Parallel()
	r := newDivergenceRig(t, "INFLIGHT")
	bin, epoch := r.carrier(r.seat.ID, "BIN-DV-INFLIGHT", "PART-A", 150)
	r.lastSeq(bin, epoch, 5)

	r.report(row(r.seat.Name, "PART-A", bin, epoch, 6, 140, 0))
	if open := r.open(); len(open) != 0 {
		t.Errorf("opened %+v while the Edge's seq 6 had not reached Core (last_seq 5)", open)
	}
}

// A LATER AGREEING REPORT CLOSES THE EPISODE as recovered. Verify-red at the
// base: nothing ever opens, so nothing closes.
func TestLinesideDivergence_AgreementClosesTheEpisode(t *testing.T) {
	t.Parallel()
	r := newDivergenceRig(t, "CLOSE")
	bin, epoch := r.carrier(r.seat.ID, "BIN-DV-CLOSE", "PART-A", 150)

	r.report(row(r.seat.Name, "PART-A", bin, epoch, 0, 10, 0))
	r.wantOne("count", bin, 10, 150)

	r.report(row(r.seat.Name, "PART-A", bin, epoch, 0, 150, 0))
	if open := r.open(); len(open) != 0 {
		t.Errorf("still open after an agreeing report: %+v", open)
	}
	if got := r.closed(); got != 1 {
		t.Errorf("%d recovered episode(s), want 1", got)
	}
}

// AN EDGE COUNTING UNDER A RETIRED GENERATION IS A DIVERGENCE whatever the
// counts say: Core drops every delta it sends as stale. Verify-red at the base.
func TestLinesideDivergence_EpochDiffers(t *testing.T) {
	t.Parallel()
	r := newDivergenceRig(t, "EPOCH")
	bin, epoch := r.carrier(r.seat.ID, "BIN-DV-EPOCH", "PART-A", 150)
	_, err := r.db.Exec(`UPDATE bins SET delta_epoch = $1 WHERE id = $2`, epoch+1, bin)
	testutil.MustNoErr(t, err, "bump core epoch")

	r.report(row(r.seat.Name, "PART-A", bin, epoch, 0, 150, 0))
	r.wantOne("epoch", bin, 150, 150)
}

// THE EDGE'S BOUND CARRIER IS ONE CORE PLACES ELSEWHERE. Verify-red at the base.
func TestLinesideDivergence_CarrierNotAtThatSeat(t *testing.T) {
	t.Parallel()
	r := newDivergenceRig(t, "ELSEWHERE")
	other := &nodes.Node{Name: "STORE_DV_ELSEWHERE", Enabled: true}
	testutil.MustNoErr(t, r.db.CreateNode(other), "create other node")
	bin, epoch := r.carrier(other.ID, "BIN-DV-ELSEWHERE", "PART-A", 150)

	r.report(row(r.seat.Name, "PART-A", bin, epoch, 0, 150, 0))
	r.wantOne("not_at_seat", bin, 150, 150)
}

// A CORE CARRIER AT A CONSUME SEAT WITH NO BOUND EDGE ROW — the SNF3 shape.
// Core has a counted carrier staged at the station's seat; the Edge reports the
// seat with its bucket only, nothing bound. Verify-red at the base.
func TestLinesideDivergence_CoreCarrierTheEdgeHasNotBound(t *testing.T) {
	t.Parallel()
	r := newDivergenceRig(t, "UNBOUND")
	bin, _ := r.carrier(r.seat.ID, "BIN-DV-UNBOUND", "PART-A", 150)

	r.report(fmt.Sprintf(`{"core_node_name":%q,"payload_code":"PART-A","bin_count":0,"bin_uop":0,"bucket_qty":0}`, r.seat.Name))
	d := r.wantOne("unbound_carrier", bin, -1, 150)
	if d.edgeCount.Valid {
		t.Errorf("edge_count = %d, want none — the Edge has no row for this carrier", d.edgeCount.Int64)
	}
}

// THE EDGE'S BUCKET DISAGREES WITH CORE'S MIRROR. Core's lineside_buckets holds
// nothing for the seat's part; the Edge reports 40 in the bucket. The episode
// has no bin, which bin_uop_exception must be able to hold. Verify-red at the
// base.
func TestLinesideDivergence_BucketDiffersFromItsMirror(t *testing.T) {
	t.Parallel()
	r := newDivergenceRig(t, "BUCKET")

	r.report(fmt.Sprintf(`{"core_node_name":%q,"payload_code":"PART-A","bin_count":0,"bin_uop":0,"bucket_qty":40}`, r.seat.Name))
	d := r.wantOne("bucket", 0, 40, 0)
	if d.binID.Valid {
		t.Errorf("bin_id = %d on a bucket episode, want NULL", d.binID.Int64)
	}
	if d.node != r.seat.Name || d.payload != "PART-A" {
		t.Errorf("episode names %s/%s, want %s/PART-A", d.node, d.payload, r.seat.Name)
	}
}

// AN EDGE THAT PREDATES THE CARRIER KEYS OPENS NOTHING about its carrier: its
// bound row says a bin is there and cannot say which, so there is nothing to
// compare. Green at the base; after the change it pins the mixed-version rule.
func TestLinesideDivergence_OldEdgeRowComparesNoCarrier(t *testing.T) {
	t.Parallel()
	r := newDivergenceRig(t, "OLDEDGE")
	r.carrier(r.seat.ID, "BIN-DV-OLDEDGE", "PART-A", 150)

	r.report(fmt.Sprintf(`{"core_node_name":%q,"payload_code":"PART-A","bin_count":1,"bin_uop":10,"bucket_qty":0}`, r.seat.Name))
	if open := r.open(); len(open) != 0 {
		t.Errorf("an old Edge's unidentified carrier opened %+v", open)
	}
}

// A REDELIVERED OR OLDER REPORT CHANGES NO EPISODE. The upsert's latest-wins
// condition moves no row for it, and the check runs only for a report that
// moved one: an hour-old report replayed after recovery must not reopen what a
// current one closed. Verify-red at the base (nothing opens in the first place).
func TestLinesideDivergence_OlderReportChangesNothing(t *testing.T) {
	t.Parallel()
	r := newDivergenceRig(t, "OLDER")
	bin, epoch := r.carrier(r.seat.ID, "BIN-DV-OLDER", "PART-A", 150)

	r.report(row(r.seat.Name, "PART-A", bin, epoch, 0, 10, 0))
	r.wantOne("count", bin, 10, 150)
	r.report(row(r.seat.Name, "PART-A", bin, epoch, 0, 150, 0))
	if got := r.closed(); got != 1 {
		t.Fatalf("%d recovered, want 1", got)
	}

	r.redeliver(row(r.seat.Name, "PART-A", bin, epoch, 0, 150, 0))
	r.at = r.at.Add(-2 * time.Minute)
	r.report(row(r.seat.Name, "PART-A", bin, epoch, 0, 10, 0))
	if open := r.open(); len(open) != 0 {
		t.Errorf("a replayed older report reopened %+v", open)
	}
}

// ── the count heals (lane N's running net), and the checksum agrees ─────────

// delta applies one BinUOPDelta through the real applier, carrying the scope's
// running net as the Edge does.
func (r *divergenceRig) delta(binID, epoch, seq int64, d int, net int64) {
	r.t.Helper()
	svc := service.NewInventoryDeltaService(r.db, service.NewBinManifestService(r.db, service.EpochAnnounce{}), service.EpochAnnounce{})
	n := net
	err := svc.ApplyBinUOPDelta(r.station, &protocol.BinUOPDelta{
		Station: r.station, BinID: binID, PayloadCode: "PART-A", Delta: d,
		Reason: protocol.ReasonConsumeTick, SequenceID: seq, Epoch: epoch, Net: &n,
		WindowStart: r.at, WindowEnd: r.at.Add(time.Duration(seq) * time.Second),
	})
	testutil.MustNoErr(r.t, err, fmt.Sprintf("apply seq %d", seq))
}

func (r *divergenceRig) coreCount(binID int64) int {
	r.t.Helper()
	var n int
	testutil.MustNoErr(r.t, r.db.QueryRow(`SELECT uop_remaining FROM bins WHERE id=$1`, binID).Scan(&n), "read count")
	return n
}

// A LOST MIDDLE DELTA HEALS, AND THE REPORT OPENS NOTHING. The Edge flushes
// three windows of one carrier (-10, -10, -5; running net -10, -20, -25) and
// the second never reaches Core. While Core is short, the report at flushed
// seq 2 is a delta in flight (Core's last_seq is 1) and opens nothing. The
// third window carries the net, Core applies -25 - (-10) = -15 and reads 125
// — the Edge's count — and the report at flushed seq 3 agrees: still nothing
// opens.
func TestLinesideDivergence_LostMiddleDeltaHealsAndOpensNothing(t *testing.T) {
	t.Parallel()
	r := newDivergenceRig(t, "HEAL")
	bin, epoch := r.carrier(r.seat.ID, "BIN-DV-HEAL", "PART-A", 150)

	r.delta(bin, epoch, 1, -10, -10)
	// seq 2 (-10, net -20) is lost.
	r.report(row(r.seat.Name, "PART-A", bin, epoch, 2, 130, 0))
	if open := r.open(); len(open) != 0 {
		t.Fatalf("Core short by the lost seq 2 opened %+v; last_seq 1 < flushed 2 is in flight", open)
	}

	r.delta(bin, epoch, 3, -5, -25)
	if got := r.coreCount(bin); got != 125 {
		t.Fatalf("Core reads %d after the healing seq 3, want 125 (150 + net -25)", got)
	}
	r.report(row(r.seat.Name, "PART-A", bin, epoch, 3, 125, 0))
	if open := r.open(); len(open) != 0 {
		t.Errorf("the healed count opened %+v", open)
	}
}

// AN EPISODE OPENED WHILE CORE WAS SHORT CLOSES WHEN THE HEAL LANDS. Core has
// claimed seq 2 but holds only the count through net -10 (140, where the Edge
// reads 130): the report at flushed seq 2 is settled and opens a count episode.
// The next window (seq 3, net -25) heals Core to 125; the report at seq 3
// agrees and the episode closes as recovered.
//
// The short state is PLANTED (the dedup row and the count written directly).
// Through the delta door lane N's net makes it unreachable — every message
// that advances last_seq also applies everything its net covers — so the only
// ways Core sits short at the Edge's own flushed seq are a write outside that
// door or a pre-net anchor. The pin is that the heal closes the episode
// however the episode came to be open.
func TestLinesideDivergence_EpisodeOpenedShortClosesWhenTheHealLands(t *testing.T) {
	t.Parallel()
	r := newDivergenceRig(t, "HEALCLOSE")
	bin, epoch := r.carrier(r.seat.ID, "BIN-DV-HEALCLOSE", "PART-A", 150)
	r.delta(bin, epoch, 1, -10, -10)
	r.lastSeq(bin, epoch, 2) // claimed seq 2; applied_net stays -10, count 140

	r.report(row(r.seat.Name, "PART-A", bin, epoch, 2, 130, 0))
	r.wantOne("count", bin, 130, 140)

	r.delta(bin, epoch, 3, -5, -25)
	if got := r.coreCount(bin); got != 125 {
		t.Fatalf("Core reads %d after the healing seq 3, want 125", got)
	}
	r.report(row(r.seat.Name, "PART-A", bin, epoch, 3, 125, 0))
	if open := r.open(); len(open) != 0 {
		t.Errorf("still open after the heal and an agreeing report: %+v", open)
	}
	if got := r.closed(); got != 1 {
		t.Errorf("%d recovered episode(s), want 1", got)
	}
}
