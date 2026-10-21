package www

import (
	"time"

	"shingoedge/domain"
	"shingoedge/engine"
	"shingoedge/service"
)

// stampStationBins stamps a station view's bins (StationService.StampBins)
// and says whether they are known (view.bins).
//
// FROM THE EDGE'S COPY WHILE CORE SERVES THE FEED, AND NOTHING ELSE. While the
// last heartbeat ack carried BinWatch the bins come from the copy of Core's
// node-bins feed this Edge holds (LocalNodeBins), with no call to Core and no
// fallback read: with nothing held yet the bins are not known. A copy Core
// has not confirmed for longer than engine.FeedAsOfAfter is drawn all the
// same, with the time it was last current; nothing is blanked for age.
// Pinned by TestStationView_BinsFromTheCopyWhileCoreServesTheFeed.
//
// OVER HTTP OTHERWISE: a Core without the feed, or no ack yet. One node-bins
// call for the station's whole list (StationService.BinNodes), pinned by
// TestStationView_OneNodeBinsReadPerBuild. This is the mixed-version path.
//
// The Production page does not come here: it keeps its own read
// (enrichViewBinState), whose count it prints.
func stampStationBins(eng ServiceAccess, stationID int64, view *domain.OperatorStationView) {
	svc := eng.StationService()
	held := eng.LocalNodeBins()
	if held.CoreSpeaksBins {
		var rows map[string]service.BoardBin
		if held.Held {
			rows = service.BoardBinsFromCopy(held.Rows)
		}
		svc.StampBins(view, rows, held.Held, held.Groups)
		view.Bins = &domain.BinsKnown{Known: held.Held, AsOf: binsAsOf(held, time.Now())}
		return
	}
	rows, read := readNodeBins(eng.CoreAPI(), svc.BinNodes(stationID))
	svc.StampBins(view, rows, read, nil)
	view.Bins = &domain.BinsKnown{Known: read}
}

// binsAsOf is when a held copy was last current, set only when that is longer
// ago than engine.FeedAsOfAfter. The copy is current as of the later of its
// arrival and Core's last confirmation.
func binsAsOf(held engine.NodeBinsState, now time.Time) *time.Time {
	if !held.Held {
		return nil
	}
	current := held.ConfirmedAt
	if held.ReceivedAt.After(current) {
		current = held.ReceivedAt
	}
	if now.Sub(current) <= engine.FeedAsOfAfter {
		return nil
	}
	at := current.UTC()
	return &at
}

// readNodeBins is one node-bins call, the rows by node name; false when
// nobody answered (no Core configured, or the call failed).
func readNodeBins(core *engine.CoreClient, names []string) (map[string]service.BoardBin, bool) {
	if core == nil || !core.Available() {
		return nil, false
	}
	bins, reachable, err := core.FetchNodeBins(names)
	if err != nil || !reachable {
		return nil, false
	}
	return service.BoardBinsFromHTTP(bins), true
}
