// HTTP boundary for the pre-flight inventory check. Edge POSTs the
// to-style required payload list; Core responds with the per-payload
// availability and the missing subset so the operator UI can refuse
// the changeover with a specific diagnostic.

package www

import (
	"context"
	"net/http"

	"shingo/protocol"

	"shingocore/asks"
)

// preflightDecodeErr is the preflight's sentence for a body that does not
// decode.
func preflightDecodeErr(err error) string { return "decode request: " + err.Error() }

// preflight is the preflight read's body, for station.
func (d *Doors) preflight(ctx context.Context, station string, req protocol.PreflightRequest) asks.Result {
	result, err := d.engine.InventoryService().PreflightAvailability(ctx, station, req.Payloads)
	if err != nil {
		return refused(http.StatusInternalServerError, "preflight: "+err.Error())
	}
	resp := protocol.PreflightResponse{Missing: result.Missing, Absent: result.Absent}
	for _, a := range result.Available {
		resp.Available = append(resp.Available, protocol.PreflightAvailability{
			PayloadCode: a.PayloadCode,
			BinCount:    a.BinCount,
		})
	}
	return accepted(resp)
}
