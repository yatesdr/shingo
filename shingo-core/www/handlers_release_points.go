package www

import (
	"shingo/protocol"

	"shingocore/asks"
)

// releasePointsDecodeErr is the release-points read's sentence for a body
// that does not decode.
func releasePointsDecodeErr(err error) string { return "release points: " + err.Error() }

// releasePoints is the release-points read's body, for station.
func (d *Doors) releasePoints(station string, req protocol.ReleasePointsRequest) asks.Result {
	if station == "" {
		return badRequest("release points: station_id is required")
	}
	return accepted(protocol.ReleasePointsResponse{
		Points: d.engine.Dispatcher().ReleasePoints(station, req.OrderUUIDs),
	})
}
