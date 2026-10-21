package www

import (
	"net/http"

	"shingo/protocol"

	"shingocore/asks"
)

// apiReleasePoints answers what releasing each named order would let its
// robot do next: the nodes its pending segment enters and the lifts it waits
// on (dispatch.ReleasePoints).
// POST /api/release/points — the adapter over Doors.releasePoints, for the
// request's station_id; the release_points ask answers through the same body
// for the asking station.
func (h *Handlers) apiReleasePoints(w http.ResponseWriter, r *http.Request) {
	serveDoor(w, r, releasePointsDecodeErr, func(req protocol.ReleasePointsRequest) asks.Result {
		return h.doors().releasePoints(req.StationID, req)
	})
}

// releasePointsDecodeErr is the release-points read's sentence for a body
// that does not decode.
func releasePointsDecodeErr(err error) string { return "release points: " + err.Error() }

// releasePoints is the release-points read's body, for station.
func (d *Doors) releasePoints(station string, req protocol.ReleasePointsRequest) asks.Result {
	req.StationID = station
	if req.StationID == "" {
		return badRequest("release points: station_id is required")
	}
	return accepted(protocol.ReleasePointsResponse{
		Points: d.engine.Dispatcher().ReleasePoints(req.StationID, req.OrderUUIDs),
	})
}
