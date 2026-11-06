//go:build docker

package dispatch

import (
	"errors"
	"strings"
	"testing"

	"shingo/protocol"
	"shingocore/fleet"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
)

// sealRefusingBackend is the simulator with the fleet refusing the "no more
// steps" call — the zero-block, complete=true ReleaseOrder — refusals times.
type sealRefusingBackend struct {
	*simulator.SimulatorBackend
	refusals int
	calls    int
}

func (b *sealRefusingBackend) ReleaseOrder(id string, blocks []fleet.OrderBlock, complete bool) error {
	if complete && len(blocks) == 0 {
		b.calls++
		if b.calls <= b.refusals {
			return errors.New("rds error 1: refused")
		}
	}
	return b.SimulatorBackend.ReleaseOrder(id, blocks, complete)
}

// TestNoWaitComplex_MarkCompleteIsRetriedThenRecorded pins H (2026-11-05): a
// no-wait complex order's "no more steps" call is retried once, and a second
// refusal goes on the order's audit trail instead of a log line alone — a fleet
// that never took it holds the order open after its last block.
//
// MUTATION: drop the retry and both cases make 1 call, not 2; drop the
// AppendAudit and the two-refusal case records no row.
func TestNoWaitComplex_MarkCompleteIsRetriedThenRecorded(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		refusals int
		wantRows int
	}{
		{"one refusal: the retry takes it", 1, 0},
		{"two refusals: recorded on the order", 2, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			sd := testdb.SetupStandardData(t, db)
			createTestBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-MC-1")
			be := &sealRefusingBackend{SimulatorBackend: simulator.New(), refusals: c.refusals}
			d, _ := newTestDispatcher(t, db, be)

			uuid := "mc-" + strings.ReplaceAll(c.name[:3], " ", "")
			d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
				OrderUUID: uuid, PayloadCode: sd.Payload.Code, Quantity: 1, ProcessNode: sd.LineNode.Name,
				Steps: []protocol.ComplexOrderStep{prPick(sd.StorageNode.Name), prDrop(sd.LineNode.Name)},
			})
			prScanPass(t, d, db, uuid)
			o := prReloadUUID(t, db, uuid)
			if o.VendorOrderID == "" {
				t.Fatalf("fixture: order %d did not dispatch (status %s, wait %q)", o.ID, o.Status, o.QueueReason)
			}
			if be.calls != 2 {
				t.Errorf("mark-complete calls = %d, want 2 (the call and one retry)", be.calls)
			}
			rows, err := db.ListEntityAudit("order", o.ID)
			if err != nil {
				t.Fatalf("list audit: %v", err)
			}
			n := 0
			for _, r := range rows {
				if r.Action == "fleet_mark_complete_failed" {
					n++
				}
			}
			if n != c.wantRows {
				t.Errorf("fleet_mark_complete_failed rows = %d, want %d", n, c.wantRows)
			}
		})
	}
}
