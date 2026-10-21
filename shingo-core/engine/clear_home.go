package engine

import (
	"errors"
	"fmt"
	"log"
	"net/http"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/store/audit"
	"shingocore/store/bins"
	"shingocore/store/cms"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
)

// clear_home.go — the Edge's one request "clear home H with partial P" (the
// clear_home ask, www.Doors.AskClearHome), and what answers it.
//
// The checks the Edge used to make before it touched anything moved here: the
// node is a pinned home of a dedicated loader, its carrier holds stock, and a
// buffer spot of the same loader holds a partial of the home's part that no
// other order holds. Then, under ONE hold of the scanner's lock (the Door), in
// this order:
//
//  1. the fence: a live clear of the home refuses the second — the ONLY 409
//     (the parent row is the key, so it survives a restart and clears itself
//     when the job ends). A request naming no clearable home is a 400;
//  2. both arms' candidates read once. With no open spot, whether the trade
//     can be born is asked too (dispatch.ClearTradeRefusal): a window that
//     cannot take the other carrier is a 400, before anything is written;
//  3. one transaction: the parent and the partial's move, the partial held,
//     the carrier zeroed (dispatch.BirthClearTx, the bin manifest's clear);
//  4. choose (dispatch.ChooseClearLocked): demand, then spot, then the trade;
//     then the parent advanced, which sends the empty's move (both trade legs
//     on the trade).
//
// Every confirm past the checks records the zero, and every answer that
// follows a zero carries the carrier's id and new epoch for the station to
// record.
//
// One round trip from the Pi: the request, the answer.

// ClearHomeAnswer is the door's answer and the HTTP status it goes with.
type ClearHomeAnswer struct {
	Status int
	Body   protocol.ClearHomeResponse
}

func clearRefused(status int, format string, a ...any) ClearHomeAnswer {
	return ClearHomeAnswer{Status: status, Body: protocol.ClearHomeResponse{
		Status: "error", Error: fmt.Sprintf(format, a...)}}
}

// ClearLoaderHome runs a home clear for an Edge station.
func (e *Engine) ClearLoaderHome(req protocol.ClearHomeRequest) ClearHomeAnswer {
	var ans ClearHomeAnswer
	e.door(func() { ans = e.clearLoaderHomeLocked(req) })
	// A refusal carries Core's current rows for the home and the spot it named,
	// read after the door, so the station redraws them before it shows the
	// sentence.
	if ans.Status != http.StatusOK {
		touched := []string{req.HomeNode}
		if req.PartialNode != "" {
			touched = append(touched, req.PartialNode)
		}
		ans.Body.Rows = e.nodeService.BinsRows(touched...)
	}
	return ans
}

func (e *Engine) clearLoaderHomeLocked(req protocol.ClearHomeRequest) ClearHomeAnswer {
	home, err := e.db.GetNodeByDotName(req.HomeNode)
	if err != nil || home == nil {
		return clearRefused(http.StatusNotFound, "node %q not found", req.HomeNode)
	}
	if live, err := e.db.LiveClearForHome(home.Name); err != nil {
		return clearRefused(http.StatusInternalServerError, "could not read whether %s is being cleared: %v", home.Name, err)
	} else if live {
		return clearRefused(http.StatusConflict, "%s is already being cleared", home.Name)
	}
	job, refusal := e.resolveClearJob(home, req)
	if refusal != nil {
		return *refusal
	}
	d := e.dispatcher
	// Both arms' candidates, read once: choose takes them as read
	// (ClearFacts.Prepared) — nothing moves them under this hold of the Door.
	pulls, err := d.ClearPulls(home, job.LoaderID)
	if err != nil {
		return clearRefused(http.StatusInternalServerError, "could not read the waiting pulls: %v", err)
	}
	spot, err := d.ClearSpot(job.LoaderID, job.Empty, job.PartialAt.Name)
	if err != nil {
		return clearRefused(http.StatusInternalServerError, "could not read the buffer: %v", err)
	}
	// NO OPEN SPOT MAY END IN THE TRADE, so whether it can be born is asked
	// before anything is written: a window that cannot take the other carrier
	// is a configuration fact, refused like the checks above. Asked whenever no
	// spot is open, pulls or not: a pull that declines the carrier leaves the
	// trade, and by then the zero is written.
	if spot == nil {
		refusal, err := d.ClearTradeRefusal(job.LoaderID, home, job.PartialAt, job.Empty, job.Partial)
		if err != nil {
			return clearRefused(http.StatusInternalServerError, "could not read whether two robots can trade at %s: %v", home.Name, err)
		}
		if refusal != "" {
			return clearRefused(http.StatusBadRequest, "no buffer spot is open and %s", refusal)
		}
	}

	birth, epoch, err := e.birthClear(job)
	if errors.Is(err, dispatch.ErrClearPartialPromised) {
		// resolveClearJob skipped every partial another order holds, under this
		// same hold of the Door; nothing should have taken one since.
		return clearRefused(http.StatusInternalServerError, "%s was promised to another order while the clear was being born", job.Partial.Label)
	}
	if err != nil {
		return clearRefused(http.StatusInternalServerError, "could not start the clear: %v", err)
	}
	d.AfterClearBirth(birth, home.Name, job.Empty.ID)
	e.notifyBinChanged(job.Empty.ID)

	empty, err := e.db.GetBin(job.Empty.ID)
	if err != nil || empty == nil {
		empty = job.Empty
	}
	arm, err := d.ChooseClearLocked(dispatch.ClearFacts{Parent: birth.Parent, Home: home,
		LoaderID: job.LoaderID, EmptyBin: empty, PartialAt: job.PartialAt.Name, Partial: birth.Partial,
		Prepared: true, Pulls: pulls, Spot: spot})
	if err != nil || arm == "" {
		// Choose could not write its arm (the trade was asked before the birth,
		// so "" is a fault here too). The job ends, nothing of it is left live,
		// and the zero stands: the answer carries it for the station to record.
		why := fmt.Sprintf("the clear could not choose where %s goes", job.Empty.Label)
		if err != nil {
			why = fmt.Sprintf("%s: %v", why, err)
		}
		d.EndClear(birth.Parent, why)
		ans := clearRefused(http.StatusInternalServerError, "%s; %s is recorded empty", why, job.Empty.Label)
		ans.Body.BinID, ans.Body.DeltaEpoch = job.Empty.ID, epoch
		return ans
	}
	if err := d.AdvanceCompoundOrder(birth.Parent.ID); err != nil {
		log.Printf("engine: clear %d: advance: %v", birth.Parent.ID, err)
	}
	return ClearHomeAnswer{Status: http.StatusOK, Body: protocol.ClearHomeResponse{
		Status: "ok", Arm: arm, ParentUUID: birth.Parent.EdgeUUID, BinID: job.Empty.ID, DeltaEpoch: epoch}}
}

// resolveClearJob makes the checks the Edge used to make, against Core's own
// records: a pinned home of a dedicated loader, one carrier on it holding
// stock that nobody has claimed, and the partial — the named spot's, else the
// first buffer spot's in the loader's order — of the home's part, held by no
// order (clearPartialOn).
func (e *Engine) resolveClearJob(home *nodes.Node, req protocol.ClearHomeRequest) (dispatch.ClearJob, *ClearHomeAnswer) {
	refuse := func(status int, format string, a ...any) (dispatch.ClearJob, *ClearHomeAnswer) {
		r := clearRefused(status, format, a...)
		return dispatch.ClearJob{}, &r
	}
	h, layout, err := e.db.GetLoaderHomeAndLayout(home.ID)
	if err != nil {
		return refuse(http.StatusInternalServerError, "could not read %s's loader: %v", home.Name, err)
	}
	if h == nil {
		return refuse(http.StatusBadRequest, "%s is not a loader position", home.Name)
	}
	if layout != protocol.LoaderLayoutDedicatedPositions {
		return refuse(http.StatusBadRequest, "%s is not a dedicated-position loader", home.Name)
	}
	if h.Kind == loaders.HomeKindBuffer || h.PayloadCode == "" {
		return refuse(http.StatusBadRequest, "%s is a buffer position, not a home", home.Name)
	}
	onHome, err := e.db.ListBinsByNode(home.ID)
	if err != nil {
		return refuse(http.StatusInternalServerError, "could not read %s: %v", home.Name, err)
	}
	if len(onHome) != 1 || onHome[0].UOPRemaining <= 0 || onHome[0].PayloadCode == "" {
		return refuse(http.StatusBadRequest, "%s has no bin with parts to clear", home.Name)
	}
	empty := onHome[0]
	if empty.ClaimedBy != nil {
		return refuse(http.StatusBadRequest, "%s is already being moved", empty.Label)
	}
	// A HELD PARTIAL IS NOT CLEARED HERE, as the bin clear's door refuses it
	// without the person's UNLOAD (apiTelemetryBinClear): an unfinished carrier
	// standing in a partial home of its own part waits to be called back to its
	// line, and the clear would zero the count that makes it worth calling. The
	// home's clear has no second tap; the station's UNLOAD is the bin clear's.
	if held, err := e.db.IsHeldPartial(empty.ID); err != nil {
		return refuse(http.StatusInternalServerError, "could not read whether %s is a held partial: %v", empty.Label, err)
	} else if held {
		return refuse(http.StatusConflict, "%s holds %d of %s, unfinished and held here to be called back to its line",
			empty.Label, empty.UOPRemaining, empty.PayloadCode)
	}
	members, err := e.db.ListLoaderHomes(h.LoaderID)
	if err != nil {
		return refuse(http.StatusInternalServerError, "could not read the loader's positions: %v", err)
	}
	named := false
	for _, m := range members {
		if m.Kind != loaders.HomeKindBuffer || m.PositionNodeID == home.ID {
			continue
		}
		spot, err := e.db.GetNode(m.PositionNodeID)
		if err != nil || spot == nil {
			continue
		}
		if req.PartialNode != "" && spot.Name != req.PartialNode {
			continue
		}
		named = true
		if p := clearPartialOn(e, spot, h.PayloadCode); p != nil {
			return dispatch.ClearJob{Station: req.Station, Home: home, LoaderID: h.LoaderID,
				Payload: h.PayloadCode, Empty: empty, Partial: p, PartialAt: spot}, nil
		}
	}
	if req.PartialNode != "" && !named {
		return refuse(http.StatusBadRequest, "%s is not a buffer position of the loader at %s", req.PartialNode, home.Name)
	}
	if req.PartialNode != "" {
		return refuse(http.StatusBadRequest, "%s holds no partial of %s", req.PartialNode, h.PayloadCode)
	}
	return refuse(http.StatusBadRequest, "no buffer spot holds a partial of %s", h.PayloadCode)
}

// clearPartialOn is the bin of the payload with parts left on spot that no
// order holds: no claim, and no soft hold either (bins.StandsUnheld). A
// partial another order has been promised is not the clear's to bring home;
// skipping it here makes that case the pick's own 400 before anything is
// written, instead of a birth the steal would refuse after the zero.
func clearPartialOn(e *Engine, spot *nodes.Node, payload string) *bins.Bin {
	on, err := e.db.ListBinsByNode(spot.ID)
	if err != nil {
		return nil
	}
	for _, b := range on {
		if b.PayloadCode != payload || b.UOPRemaining <= 0 || b.ClaimedBy != nil {
			continue
		}
		if free, err := e.db.BinStandsUnheld(b.ID, spot.Name); err == nil && free {
			return b
		}
	}
	return nil
}

// birthClear is step 3's one transaction: the CMS departure the zero owes
// (if the home is a CMS boundary), the parent and the partial's move, and the
// carrier's zero — all or nothing. Returns the carrier's new delta epoch.
func (e *Engine) birthClear(job dispatch.ClearJob) (*dispatch.ClearBirth, int64, error) {
	txns := e.buildClearDeparture(job.Empty.ID, job.Home.ID)
	tx, err := e.db.Begin()
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback() //nolint:errcheck // committed below; rollback is the error close
	if len(txns) > 0 {
		if err := cms.CreateInTx(tx, txns); err != nil {
			return nil, 0, fmt.Errorf("book the cms departure of %s: %w", job.Empty.Label, err)
		}
	}
	birth, err := e.dispatcher.BirthClearTx(tx, job)
	if err != nil {
		return nil, 0, err
	}
	epoch, err := e.binManifest.ClearForReuseTx(tx, job.Empty.ID, nil, audit.OpClearForReuse,
		"engine/clear_home.go:ClearLoaderHome", protocol.DeclaredByLifecycle)
	if err != nil {
		return nil, 0, err
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, err
	}
	if len(txns) > 0 {
		e.Events.Emit(Event{Type: EventCMSTransaction, Payload: CMSTransactionEvent{Transactions: txns}})
	}
	return birth, epoch, nil
}
