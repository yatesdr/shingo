//go:build docker

package dispatch

import (
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
)

// release_l11_docker_test.go — L11: a refused release writes nothing of Core's.
// The manifest sync writes the operator's count onto the bin; it ran above the
// gate fence and the wait-index refusal, so a release Core then refused had
// already overwritten the bin's count.

func TestReleaseL11_RefusalsWriteNoCount(t *testing.T) {
	t.Parallel()
	laneFirst := []resolvedStep{
		{Action: protocol.ActionWait, Node: "SYN-LANE-PT", WaitKind: WaitKindLane},
		vsPick("SYN-LANE-SLOT"), vsDrop("SYN-PRESS"),
	}
	single := []resolvedStep{rlWait("SYN-PRESS", WaitKindStation), vsPick("SYN-PRESS"), vsDrop("SYN-OUT")}
	for _, c := range []struct {
		name      string
		steps     []resolvedStep
		waitIndex int
		bug       string
		today     string
		want      string
	}{
		{"staged on a lane's gate wait", laneFirst, 0, "",
			"", "errors=[invalid_state] bin_uop=30"},
		{"staged past its last wait", single, 1, "",
			"", "errors=[invalid_state] bin_uop=30"},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newReleaseRig(t)
			sd := testdb.SetupStandardData(t, r.db)
			bin := testdb.CreateBinAtNode(t, r.db, sd.Payload.Code, sd.LineNode.ID, "SYN-BIN-L11")
			testutil.MustNoErr(t, r.db.SetBinManifest(bin.ID, `{"items":[{"part_number":"PART-X"}]}`, sd.Payload.Code, 30), "seed count")
			o := rlLeg(t, r.db, "l11-leg", StatusStaged, c.waitIndex, string(mustJSON(t, c.steps)))
			testutil.MustNoErr(t, r.db.UpdateOrderBinID(o.ID, bin.ID), "bind bin")
			_, err := r.db.Exec(`UPDATE bins SET claimed_by=$1 WHERE id=$2`, o.ID, bin.ID)
			testutil.MustNoErr(t, err, "claim bin for the leg")
			zero := 0
			errsBefore := len(orderErrorCodes(t, r.db, "l11-leg"))
			r.d.HandleOrderRelease(r.d.syntheticEnvelope("line-1"), &protocol.OrderRelease{OrderUUID: "l11-leg", RemainingUOP: &zero})
			b, err := r.db.GetBin(bin.ID)
			testutil.MustNoErr(t, err, "reload bin")
			got := fmt.Sprintf("errors=[%s] bin_uop=%d",
				strings.Join(orderErrorCodes(t, r.db, "l11-leg")[errsBefore:], ","), b.UOPRemaining)
			pinOutcome(t, c.bug, got, c.today, c.want)
		})
	}
}
