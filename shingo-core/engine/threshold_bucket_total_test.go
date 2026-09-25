//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/store/plantclaims"
)

// threshold_bucket_total_test.go — a lineside bucket's qty moves the total every
// threshold door decides against, and the ask sizing input with it.
//
// The rig's bin holds 40 against a threshold of 100. A bucket row for the same
// payload is planted at a seat, and each door is driven the way production
// drives it. Where the bucket counts, 40 + qty is the total the door judges and
// the CurrentUOP the fire carries into ReplenishLoader → BinsToReachThreshold.

// rtBucketDoors is every door into the fire gate, including the bucket door
// (OnBucketApplied) that rtFeeders does not list.
var rtBucketDoors = []struct {
	name  string
	prime bool
	drive func(r *rtRig)
}{
	{"delta", true, func(r *rtRig) { r.delta() }},
	{"bucket", true, func(r *rtRig) {
		r.m.OnBucketApplied(r.b.stationID, "ALN-BKT-RT", r.b.payloadCode, -1, protocol.ReasonConsumeDrain)
	}},
	{"manual_swap", true, func(r *rtRig) { r.m.NoteSwapRequestContradiction(r.b.payloadCode) }},
	{"startup_sweep", false, func(r *rtRig) { r.boot() }},
	{"resync", false, func(r *rtRig) { r.m.Resync(r.b.stationID) }},
}

// plantBucket writes one lineside_buckets row for the rig's payload at node.
func (r *rtRig) plantBucket(node string, qty int) {
	r.t.Helper()
	_, err := r.eng.db.Exec(`INSERT INTO lineside_buckets (station, core_node_name, pair_key, style_id, payload_code, qty)
		VALUES ('PLANT.RT', $1, 'PK', 1, $2, $3)`, node, r.b.payloadCode, qty)
	testutil.MustNoErr(r.t, err, "plant bucket")
}

// strandByClaims makes node run an active style whose claim consumes a
// different part, so the claims-derived rule reads a bucket there as stranded.
func (r *rtRig) strandByClaims(node string) {
	r.t.Helper()
	proc := "PROC-" + node
	testutil.MustNoErr(r.t, plantclaims.ReplaceProcess(r.eng.db.DB, proc,
		[]plantclaims.StyleRow{{ProcessID: proc, StyleID: "S-NOW", ConfigGen: 1, IsActive: true}},
		[]plantclaims.ClaimRow{{ProcessID: proc, StyleID: "S-NOW", CoreNodeName: node,
			Role: protocol.ClaimRoleConsume, PayloadCode: "OTHER-" + r.b.payloadCode}}, 0), "seed claims")
}

// TestReadThrough_BucketQtyMovesTheDecisionAtEveryDoor: at every door,
//   - an active bucket that lifts the total to or above the threshold HOLDS
//     (40 + 80 = 120): stays under the change (an active pile counts, #1);
//   - an active bucket that leaves it below FIRES, and the fire carries the
//     bucket in CurrentUOP (40 + 30 = 70), the input the ask is sized from:
//     stays;
//   - a bucket at a seat whose active style consumes another part is
//     claims-stranded and does not count (fires at 40): flips under brief v7
//     expected change #3 (stranded becomes a state set at cutover; a pile that
//     was never stranded counts, so this seat holds at 120).
func TestReadThrough_BucketQtyMovesTheDecisionAtEveryDoor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		qty      int
		stranded bool
		wantFire bool
		wantUOP  int
	}{
		{"active_holds", 80, false, false, 0},
		{"active_sizes", 30, false, true, 70},
		{"claims_stranded_excluded", 80, true, true, 40},
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
				if c.stranded {
					r.strandByClaims(seat)
				}
				r.plantBucket(seat, c.qty)
				d.drive(r)

				fired := r.fired()
				if !c.wantFire {
					if len(fired) != 0 || len(r.openRows()) != 0 {
						t.Errorf("bin 40 + bucket %d: fired=%d open=%d, want neither", c.qty, len(fired), len(r.openRows()))
					}
					return
				}
				if len(fired) != 1 {
					t.Fatalf("bin 40 + bucket %d: fired %d time(s), want 1", c.qty, len(fired))
				}
				if fired[0].CurrentUOP != c.wantUOP {
					t.Errorf("fire carried CurrentUOP=%d, want %d", fired[0].CurrentUOP, c.wantUOP)
				}
			})
		}
	}
}
