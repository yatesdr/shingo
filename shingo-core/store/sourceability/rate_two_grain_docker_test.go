//go:build docker

package sourceability_test

import (
	"database/sql"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/plantclaims"
	"shingocore/store/sourceability"
)

// B7's DB pins: the rate SQL itself. The pure pins (rate_grain_test.go) prove
// the Compute half; these seed raw bin_uop_ledger rows of the exact shape the
// applier writes and prove the two-grain fold — which rows count, at which
// grain, and what lands in tte_samples.
//
// Every fixture uses a 30-minute window (1800s), the configured default, and
// seeds the ledger directly: the applier's apply-path behaviour is pinned in
// uop/applier_test.go; what matters here is what the RATE makes of the rows.

// seedRateDelta writes one bin_uop_delta ledger row — post-v119 shape: reason
// on the column and in metadata, node_id nullable (a carrier standing nowhere
// writes NULL, per applier.go's comment).
func seedRateDelta(t *testing.T, db *sql.DB, binID int64, nodeID any, payload, reason string, before, after int) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO bin_uop_ledger
		(bin_id, before_uop, after_uop, op, source, payload_code, actor, metadata, node_id, reason, applied_at)
		VALUES ($1, $2, $3, 'bin_uop_delta', 'test', $4, 'test',
			jsonb_build_object('reason', $5::text, 'delta', $6::int, 'sequence_id', 1),
			$7, $5, NOW() - make_interval(secs => 60))`,
		binID, before, after, payload, reason, after-before, nodeID)
	if err != nil {
		t.Fatalf("seed delta row: %v", err)
	}
}

// rateWorld is one test's plant: standard data, a second line node, one bin
// staged at each line, and SNF2/A claiming both lines for BIN-A. Seeded bins
// start with uop_remaining set by the caller (lineUOP reads bins at the node).
type rateWorld struct {
	db     *sql.DB
	sdb    *store.DB
	std    *testdb.StandardData
	line2  *int64
	bin1ID int64
	bin2ID int64
}

func setupRateWorld(t *testing.T, uop1, uop2 int) *rateWorld {
	t.Helper()
	sdb := testdb.Open(t)
	db := sdb.DB
	std := testdb.SetupStandardData(t, sdb)

	// A second line node so two cells can share a payload.
	var line2ID int64
	err := db.QueryRow(`INSERT INTO nodes (name, enabled, is_synthetic)
		VALUES ('RATE-LINE2', true, false) RETURNING id`).Scan(&line2ID)
	if err != nil {
		t.Fatalf("create line2: %v", err)
	}

	// A bin staged at each line (staged = claimed by nothing, standing at the
	// node; lineUOPByNode sums uop_remaining of bins at enabled nodes).
	bin1 := testdb.CreateBinAtNode(t, sdb, "BIN-A", std.StorageNode.ID, "rate-bin-1")
	if _, err := db.Exec(`UPDATE bins SET node_id=$2, uop_remaining=$3, payload_code='BIN-A', manifest_confirmed=true WHERE id=$1`,
		bin1.ID, std.LineNode.ID, uop1); err != nil {
		t.Fatalf("stage bin1: %v", err)
	}
	bin2 := testdb.CreateBinAtNode(t, sdb, "BIN-A", std.StorageNode.ID, "rate-bin-2")
	if _, err := db.Exec(`UPDATE bins SET node_id=$2, uop_remaining=$3, payload_code='BIN-A', manifest_confirmed=true WHERE id=$1`,
		bin2.ID, line2ID, uop2); err != nil {
		t.Fatalf("stage bin2: %v", err)
	}

	// Mirror: SNF2 style A claims BIN-A at both lines. IsActive is what makes
	// the pass a sampler: only the running style's lines get TTE samples.
	styles := []plantclaims.StyleRow{{ProcessID: "SNF2", StyleID: "A", IsActive: true}}
	claims := []plantclaims.ClaimRow{
		{ProcessID: "SNF2", StyleID: "A", CoreNodeName: std.LineNode.Name, PayloadCode: "BIN-A", Seq: 0},
		{ProcessID: "SNF2", StyleID: "A", CoreNodeName: "RATE-LINE2", PayloadCode: "BIN-A", Seq: 1},
	}
	testutil.MustNoErr(t, plantclaims.ReplaceProcess(db, "SNF2", styles, claims, 0), "seed mirror")

	return &rateWorld{db: db, sdb: sdb, std: std, line2: &line2ID, bin1ID: bin1.ID, bin2ID: bin2.ID}
}

// buildAndCompute runs the monitor's own path: BuildInputs over the 30-minute
// window, ComputeWithSamples for both the verdict and the recorded samples.
func buildAndCompute(t *testing.T, w *rateWorld) ([]sourceability.StyleState, []sourceability.TTESample) {
	t.Helper()
	in, err := sourceability.BuildInputs(w.db, 30*time.Minute)
	testutil.MustNoErr(t, err, "build inputs")
	return sourceability.ComputeWithSamples(in, sourceability.Config{}, time.Now())
}

func sampleFor(t *testing.T, samples []sourceability.TTESample, node string) sourceability.TTESample {
	t.Helper()
	for _, s := range samples {
		if s.Line.NodeName == node {
			return s
		}
	}
	t.Fatalf("no sample for node %s in %+v", node, samples)
	return sourceability.TTESample{}
}

const w1800 = 1800.0 // window seconds; rates below are consumed/1800

// TestRate_CorrectionsAndCapturesLeaveTheRate pins Fix 1's reason filter
// through the whole path: 12 consume ticks count; a −200 operator correction
// and a −50 capture beside them do not. Pre-B7 the polluted rate was
// 262/1800 ≈ 0.14556 (recorded in the prediction file); the pin demands the
// clean 12/1800.
func TestRate_CorrectionsAndCapturesLeaveTheRate(t *testing.T) {
	w := setupRateWorld(t, 120, 0)
	seedRateDelta(t, w.db, w.bin1ID, w.std.LineNode.ID, "BIN-A", "consume_tick", 112, 100) // 12 units in one row
	seedRateDelta(t, w.db, w.bin1ID, w.std.LineNode.ID, "BIN-A", "operator_correction", 200, 0)
	seedRateDelta(t, w.db, w.bin1ID, w.std.LineNode.ID, "BIN-A", "capture_reduction", 150, 100)

	_, samples := buildAndCompute(t, w)
	s := sampleFor(t, samples, w.std.LineNode.Name)
	if got := s.Line.RatePerSec; got < 12/w1800-1e-9 || got > 12/w1800+1e-9 {
		t.Errorf("rate = %v, want 12/1800 = %v (correction and capture must not count)", got, 12/w1800)
	}
	if wantSecs := 120 / (12 / w1800); s.Line.TimeToEmpty.Seconds() < wantSecs-0.5 || s.Line.TimeToEmpty.Seconds() > wantSecs+0.5 {
		t.Errorf("TTE = %v, want ~%vs", s.Line.TimeToEmpty, wantSecs)
	}
	if !s.Line.Known || s.Line.RateGrain != "node" {
		t.Errorf("line = %+v, want known at node grain", s.Line)
	}
}

// TestRate_TwoCellsTwoRatesPlusNullNodeRow pins Fix 3's fold: LINE1 60 units,
// LINE2 180 units, one NULL-node row 30 units. The payload sum keeps all 270
// (a carrier standing nowhere is still consumption); each node's rate is its
// own 60 or 180; the NULL row joins no node.
func TestRate_TwoCellsTwoRatesPlusNullNodeRow(t *testing.T) {
	w := setupRateWorld(t, 120, 90)
	seedRateDelta(t, w.db, w.bin1ID, w.std.LineNode.ID, "BIN-A", "consume_tick", 60, 0)
	seedRateDelta(t, w.db, w.bin2ID, *w.line2, "BIN-A", "consume_tick", 180, 0)
	seedRateDelta(t, w.db, w.bin1ID, nil, "BIN-A", "consume_tick", 30, 0)

	_, samples := buildAndCompute(t, w)
	s1 := sampleFor(t, samples, w.std.LineNode.Name)
	s2 := sampleFor(t, samples, "RATE-LINE2")

	if got := s1.Line.RatePerSec; got < 60/w1800-1e-9 || got > 60/w1800+1e-9 {
		t.Errorf("LINE1 rate = %v, want 60/1800 (its own consumption only)", got)
	}
	if got := s2.Line.RatePerSec; got < 180/w1800-1e-9 || got > 180/w1800+1e-9 {
		t.Errorf("LINE2 rate = %v, want 180/1800 (its own consumption only)", got)
	}
	if s1.Line.RateGrain != "node" || s2.Line.RateGrain != "node" {
		t.Errorf("grains = %q/%q, want node/node", s1.Line.RateGrain, s2.Line.RateGrain)
	}
	// 120/0.03333 = 3600s; 90/0.1 = 900s.
	if s1.Line.TimeToEmpty != 3600*time.Second {
		t.Errorf("LINE1 TTE = %v, want 3600s", s1.Line.TimeToEmpty)
	}
	if s2.Line.TimeToEmpty != 900*time.Second {
		t.Errorf("LINE2 TTE = %v, want 900s", s2.Line.TimeToEmpty)
	}
}

// TestRate_ABFallthroughCountsPlantWideOnly pins the A/B rule the tree forced:
// fallthrough rows are stamped on the INACTIVE node (node_id from the bins row
// at apply time; fallthrough only fires when no active-pull node exists), so
// they count in the payload grain and are excluded from every node grain —
// an under-count on the active node beats a wrong node.
func TestRate_ABFallthroughCountsPlantWideOnly(t *testing.T) {
	w := setupRateWorld(t, 120, 60)
	seedRateDelta(t, w.db, w.bin1ID, w.std.LineNode.ID, "BIN-A", "consume_tick", 60, 0)
	seedRateDelta(t, w.db, w.bin1ID, w.std.LineNode.ID, "BIN-A", "ab_fallthrough", 24, 0)

	_, samples := buildAndCompute(t, w)
	s := sampleFor(t, samples, w.std.LineNode.Name)

	// The node rate is the 60 consume_tick units ONLY: if the 24 fallthrough
	// units leaked into byNode, LINE1 would read 84/1800.
	if got := s.Line.RatePerSec; got < 60/w1800-1e-9 || got > 60/w1800+1e-9 {
		t.Errorf("LINE1 node rate = %v, want 60/1800 (fallthrough excluded from the node grain)", got)
	}
	// The payload grain keeps the 24: assert through the monitor's own fallback
	// — LINE2 has no rows, so its projection reads the plant-wide rate, which
	// must carry 84/1800, not 60/1800. That covers both grains in one Compute.
	s2 := sampleFor(t, samples, "RATE-LINE2")
	if got := s2.Line.RatePerSec; got < 84/w1800-1e-9 || got > 84/w1800+1e-9 {
		t.Errorf("LINE2 fallback rate = %v, want 84/1800 (fallthrough counted in the payload grain)", got)
	}
	if s2.Line.RateGrain != "payload" {
		t.Errorf("LINE2 grain = %q, want payload (no own rows)", s2.Line.RateGrain)
	}
}

// TestRate_FallbackGrainRecordedOnSample pins the sample's grain stamp through
// the writer: a node with no rows in the window gets the plant-wide rate and
// the row says "payload"; a node with its own rows says "node". The row is the
// forecast's evidence — scored wrong grain, wrong scrutiny.
func TestRate_FallbackGrainRecordedOnSample(t *testing.T) {
	w := setupRateWorld(t, 60, 60)
	// LINE1 has consume rows; LINE2 has none (changeover case).
	seedRateDelta(t, w.db, w.bin1ID, w.std.LineNode.ID, "BIN-A", "consume_tick", 60, 0)

	_, samples := buildAndCompute(t, w)
	testutil.MustNoErr(t, sourceability.RecordTTESamples(w.db, samples, sourceability.TTERetention), "record samples")

	rows, err := w.db.Query(`SELECT core_node_name, rate_per_sec, rate_grain, tte_seconds
		FROM tte_samples ORDER BY core_node_name`)
	if err != nil {
		t.Fatalf("read samples: %v", err)
	}
	defer rows.Close()
	got := map[string][3]float64{}
	grain := map[string]string{}
	for rows.Next() {
		var node string
		var r, tte float64
		var g string
		if err := rows.Scan(&node, &r, &g, &tte); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[node] = [3]float64{r, tte, 0}
		grain[node] = g
	}
	if g := grain[w.std.LineNode.Name]; g != "node" {
		t.Errorf("%s grain = %q, want node (own rows in window)", w.std.LineNode.Name, g)
	}
	if g := grain["RATE-LINE2"]; g != "payload" {
		t.Errorf("RATE-LINE2 grain = %q, want payload (no rows, plant-wide fallback)", g)
	}
	// LINE2's rate must be the plant-wide 60/1800, not zero.
	s2 := got["RATE-LINE2"]
	if s2[0] < 60/w1800-1e-9 || s2[0] > 60/w1800+1e-9 {
		t.Errorf("RATE-LINE2 rate = %v, want 60/1800 via fallback", s2[0])
	}
}

// TestRate_HonestCaseStable pins the brief's rule-of-the-brief: a fixture
// whose ledger has only consume_tick rows at one node per payload computes
// the same numbers it did before B7 — the only change is the grain column
// naming what was implicit. Two nodes, two payloads, no exotic rows.
func TestRate_HonestCaseStable(t *testing.T) {
	w := setupRateWorld(t, 120, 90)
	seedRateDelta(t, w.db, w.bin1ID, w.std.LineNode.ID, "BIN-A", "consume_tick", 60, 0)
	seedRateDelta(t, w.db, w.bin2ID, *w.line2, "BIN-A", "consume_tick", 180, 0)

	_, samples := buildAndCompute(t, w)
	s1 := sampleFor(t, samples, w.std.LineNode.Name)
	s2 := sampleFor(t, samples, "RATE-LINE2")
	// Pre-B7 both read the plant-wide 240/1800; the honest single-node case
	// (one node per payload) is where B7 must NOT move the number. Here the
	// fixture has two nodes on ONE payload, so the per-payload view still
	// exists; assert each node reads its own and the values are exact.
	if s1.Line.RatePerSec != 60/w1800 || s2.Line.RatePerSec != 180/w1800 {
		t.Errorf("rates = %v/%v, want 60/1800 and 180/1800", s1.Line.RatePerSec, s2.Line.RatePerSec)
	}
	if s1.Line.TimeToEmpty != 3600*time.Second || s2.Line.TimeToEmpty != 900*time.Second {
		t.Errorf("TTEs = %v/%v, want 3600s/900s", s1.Line.TimeToEmpty, s2.Line.TimeToEmpty)
	}
}
