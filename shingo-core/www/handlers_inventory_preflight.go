// The preflight read-ask's body: the flow composer's draft parts, answered with
// the ones that have no bin free (Missing) and no bin at all (Absent), which
// the confirm sheet says apart.

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

// preflight is the preflight read's body. Plant-wide: the asking station
// scopes nothing.
func (d *Doors) preflight(ctx context.Context, req protocol.PreflightRequest) asks.Result {
	result, err := d.engine.InventoryService().PreflightAvailability(ctx, req.Payloads)
	if err != nil {
		return refused(http.StatusInternalServerError, "preflight: "+err.Error())
	}
	return accepted(protocol.PreflightResponse{Missing: result.Missing, Absent: result.Absent})
}
