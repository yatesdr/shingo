//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/store/plantclaims"
)

// threshold_bucket_total_test.go — an active lineside pile's qty moves the total
// every threshold door decides against, and the ask sizing input with it; a
// stranded row of the same payload moves neither.
//
// The rig's bin holds 40 against a threshold of 100. Pile rows for the same
// payload are planted at a seat, and each door is driven the way production
// drives it. Where a pile counts, 40 + qty is the total the door judges and
// the CurrentUOP the fire carries into ReplenishLoader → BinsToReachThreshold.

// rtBucketDoors is every door into the fire gate, including the bucket door
// (OnBucketApplied) that rtFeeders does not list.
var rtBucketDoors = []struct {
	name  string
	prime bool
	drive func(r *rtRig)
}{
	{"delta", true, func(r *rtRig) { r.delta() }},
	{"bucket", true, func(r *rtRig) { r.m.OnBucketApplied(r.b.payloadCode) }},
	{"manual_swap", true, func(r *rtRig) { r.m.NoteSwapRequestContradiction(r.b.payloadCode) }},
	{"startup_sweep", false, func(r *rtRig) { r.boot() }},
	{"resync", false, func(r *rtRig) { r.m.Resync(r.b.stationID) }},
}

// plantBucket writes one lineside_buckets row for the rig's payload at node.
func (r *rtRig) plantBucket(node string, state protocol.LinesideBucketState, qty int) {
	r.t.Helper()
	_, err := r.eng.db.Exec(`INSERT INTO lineside_buckets (station, core_node_name, payload_code, state, qty)
		VALUES ('PLANT.RT', $1, $2, $3, $4)`, node, r.b.payloadCode, string(state), qty)
	testutil.MustNoErr(r.t, err, "plant bucket")
}

// claimAnotherPart makes node run an active style whose claim consumes a
// different part: the shape the retired claims-derived rule read as stranded.
func (r *rtRig) claimAnotherPart(node string) {
	r.t.Helper()
	proc := "PROC-" + node
	testutil.MustNoErr(r.t, plantclaims.ReplaceProcess(r.eng.db.DB, proc,
		[]plantclaims.StyleRow{{ProcessID: proc, StyleID: "S-NOW", ConfigGen: 1, IsActive: true}},
		[]plantclaims.ClaimRow{{ProcessID: proc, StyleID: "S-NOW", CoreNodeName: node,
			Role: protocol.ClaimRoleConsume, PayloadCode: "OTHER-" + r.b.payloadCode}}, 0), "seed claims")
}

// TestReadThrough_BucketQtyMovesTheDecisionAtEveryDoor: at every door,
//   - an active pile that lifts the total to or above the threshold HOLDS
//     (40 + 80 = 120): stays under the change (an active pile counts, #1);
//   - an active pile that leaves it below FIRES, and the fire carries the
//     pile in CurrentUOP (40 + 30 = 70), the input the ask is sized from:
//     stays;
//   - an active pile at a seat whose active style consumes another part
//     COUNTS and holds (40 + 80 = 120). This case was
//     claims_stranded_excluded (fired at 40, the claims-derived rule reading
//     the pile as stranded); FLIPPED BY BRIEF v7 EXPECTED CHANGE #3: stranded
//     is a state set at cutover, and a pile that was never stranded counts;
//   - a stranded row beside an active pile of the same payload at the same
//     seat does not count: fires at 40 + 30 = 70 with 80 stranded (new).
func TestReadThrough_BucketQtyMovesTheDecisionAtEveryDoor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		active      int
		stranded    int
		claimsOther bool
		wantFire    bool
		wantUOP     int
	}{
		{"active_holds", 80, 0, false, false, 0},
		{"active_sizes", 30, 0, false, true, 70},
		{"unclaimed_active_counts", 80, 0, true, false, 0},
		{"stranded_row_excluded", 30, 80, false, true, 70},
	}
	for _, d := range rtBucketDoors {
		for _, c := range cases {
			t.Run(d.name+"/"+c.name, func(t *testing.T) {
				t.Parallel()
				r := newRTRig(t, "PANEL-RT-BKT-"+d.name+"-"+c.name, 500)
				if d.prime {
					r.boot()
				}
				r.setUOP(40)
				const seat = "ALN-BKT-RT"
				if c.claimsOther {
					r.claimAnotherPart(seat)
				}
				r.plantBucket(seat, protocol.LinesideBucketActive, c.active)
				if c.stranded > 0 {
					r.plantBucket(seat, protocol.LinesideBucketStranded, c.stranded)
				}
				d.drive(r)

				fired := r.fired()
				if !c.wantFire {
					if len(fired) != 0 || len(r.openRows()) != 0 {
						t.Errorf("bin 40 + active %d: fired=%d open=%d, want neither", c.active, len(fired), len(r.openRows()))
					}
					return
				}
				if len(fired) != 1 {
					t.Fatalf("bin 40 + active %d (+ stranded %d): fired %d time(s), want 1", c.active, c.stranded, len(fired))
				}
				if fired[0].CurrentUOP != c.wantUOP {
					t.Errorf("fire carried CurrentUOP=%d, want %d", fired[0].CurrentUOP, c.wantUOP)
				}
			})
		}
	}
}
