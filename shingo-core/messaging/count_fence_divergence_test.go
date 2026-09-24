//go:build docker

package messaging

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/service"
	"shingocore/store/payloads"
)

// THE FENCED COUNT AGREES WITH THE REPORT AT THE SAME SEQ (the record-count
// fence meets lane A's classifier, which compares a count only when Core's
// last_seq equals the report's FlushedSeq).
//
// Core holds 140 after seq 1 (net -10). The operator counts 100 while seq 2
// (-5, net -15) is in flight. Core writes 100 with the fence {net -10, seq 1};
// the station rebases: 100 + (-15 - -10) + 0 = 95. Its report at flushed seq 2
// while Core is still at last_seq 1 is in flight and compares nothing; once
// seq 2 lands Core reads 100 + (-15 - -10) = 95, the report at seq 2 agrees,
// and no episode opens.
//
// The control is the pre-fence station, which writes Core's 100 as is: its
// report at seq 2 says 100 against Core's 95, and the classifier opens a count
// episode. That is the P0f divergence, and it is what the fence removes.
func TestLinesideDivergence_FencedCountAgreesAtTheSameSeq(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		edge     int
		episodes int
	}{
		{"fenced station rebases", 95, 0},
		{"unfenced station takes Core's number", 100, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newDivergenceRig(t, "FENCE"+string(rune('A'+tc.episodes)))
			bin, epoch := r.carrier(r.seat.ID, "BIN-DV-FENCE", "PART-A", 150)
			r.delta(bin, epoch, 1, -10, -10)

			// RecordCount validates against the payload's capacity.
			if _, err := r.db.GetPayloadByCode("PART-A"); err != nil {
				testutil.MustNoErr(t, r.db.CreatePayload(&payloads.Payload{Code: "PART-A", UOPCapacity: 1000}), "create payload")
			}
			bins := service.NewBinService(r.db, service.NewBinManifestService(r.db, service.EpochAnnounce{Topic: "t", CoreStation: "core.test"}))
			b, err := r.db.GetBin(bin)
			testutil.MustNoErr(t, err, "read bin")
			res, err := bins.RecordCount(b, 100, "operator-under-test")
			testutil.MustNoErr(t, err, "RecordCount")
			if res.AsOfNet == nil || *res.AsOfNet != -10 || res.AsOfSeq == nil || *res.AsOfSeq != 1 || res.AsOfStation != r.station {
				t.Fatalf("fence = {%v, %v, %q}, want {-10, 1, %s}", res.AsOfNet, res.AsOfSeq, res.AsOfStation, r.station)
			}

			// The station's report at flushed seq 2, before Core has seq 2.
			r.report(row(r.seat.Name, "PART-A", bin, epoch, 2, tc.edge, 0))
			if open := r.open(); len(open) != 0 {
				t.Fatalf("a report ahead of Core opened %+v; last_seq 1 < flushed 2 is in flight", open)
			}

			r.delta(bin, epoch, 2, -5, -15)
			if got := r.coreCount(bin); got != 95 {
				t.Fatalf("Core reads %d after seq 2, want 95", got)
			}
			r.report(row(r.seat.Name, "PART-A", bin, epoch, 2, tc.edge, 0))
			if open := r.open(); len(open) != tc.episodes {
				t.Errorf("open episodes = %+v, want %d", open, tc.episodes)
			}
		})
	}
}
