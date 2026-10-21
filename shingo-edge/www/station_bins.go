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
// FROM THE EDGE'S COPY, AND NOTHING ELSE. The bins come from the copy of
// Core's node-bins feed this Edge holds (LocalNodeBins), with no call to Core
// and no fallback read: with nothing held the bins are not known — before the
// first snapshot, and always against a Core without the feed (the HTTP read
// that served one went with this door). A copy Core has not confirmed for
// longer than engine.FeedAsOfAfter is drawn all the same, with the time it was
// last current; nothing is blanked for age. Pinned by
// TestStationView_BinsFromTheCopyOnly.
//
// The Production page does not come here: it reads the same copy by the same
// display rule (enrichViewBinState) and, having no "as of" to show, draws no
// bin from a copy that rule calls old.
func stampStationBins(eng ServiceAccess, view *domain.OperatorStationView) {
	held := eng.LocalNodeBins()
	var rows map[string]service.BoardBin
	if held.Held {
		rows = service.BoardBinsFromCopy(held.Rows)
	}
	eng.StationService().StampBins(view, rows, held.Held, held.Groups)
	view.Bins = &domain.BinsKnown{Known: held.Held, AsOf: binsAsOf(held, time.Now())}
}

// binsAsOf is when a held copy was last current, set only when that is longer
// ago than engine.FeedAsOfAfter (engine.CurrentAt, the screens' rule).
func binsAsOf(held engine.NodeBinsState, now time.Time) *time.Time {
	if !held.Held {
		return nil
	}
	current, old := engine.CurrentAt(held.ReceivedAt, held.ConfirmedAt, now)
	if !old {
		return nil
	}
	at := current.UTC()
	return &at
}
