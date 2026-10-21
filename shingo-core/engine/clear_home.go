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

// clear_home.go — the Edge's one request "clear home H with partial P"
// (POST /api/telemetry/clear-home), and the door that answers it.
//
// The checks the Edge used to make before it touched anything moved here: the
// node is a pinned home of a dedicated loader, its carrier holds stock, and a
// buffer spot of the same loader holds a partial of the home's part. Then,
// under ONE hold of the scanner's lock (the Door), in this order:
//
//  1. the fence: a live clear of the home refuses the second — the ONLY 409
//     (the parent row is the key, so it survives a restart and clears itself
//     when the job ends). A request naming no clearable home is a 400;
//  2. both arms' candidates read once. No pull and no open spot is the
//     two-robot trade, not built yet (owner, STOP 6: the gap the pair brief
//     closes): the zero is recorded, no job is born, and the answer is the arm
//     "none" with ClearPartialStays;
//  3. one transaction: the parent and the partial's move, the partial held,
//     the carrier zeroed (dispatch.BirthClearTx, the bin manifest's clear);
//  4. choose (dispatch.ChooseClearLocked): demand, then spot. With neither —
//     candidates existed but none took the carrier — the job ends and the
//     answer is the same "none": the zero stands.
//
// Every confirm past the fence and the checks records the zero, and every
// answer carries the carrier's id and new epoch for the station to record.
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
	// Both arms' candidates, read once: the refusal below needs them, and choose
	// takes them as read (ClearFacts.Prepared) — nothing moves them under this
	// hold of the Door.
	pulls, err := d.ClearPulls(home, job.LoaderID)
	if err != nil {
		return clearRefused(http.StatusInternalServerError, "could not read the waiting pulls: %v", err)
	}
	spot, err := d.ClearSpot(job.LoaderID, job.Empty, job.PartialAt.Name)
	if err != nil {
		return clearRefused(http.StatusInternalServerError, "could not read the buffer: %v", err)
	}
	// THE GAP THE PAIR BRIEF CLOSES (owner, STOP 6): no pull and no open spot is
	// the two-robot trade, which is not built. The zero is recorded (the floor
	// fact) and no job is born; the station is told the partial stays.
	if spot == nil && len(pulls) == 0 {
		return e.clearZeroOnly(job)
	}

	birth, epoch, err := e.birthClear(job)
	if errors.Is(err, dispatch.ErrClearPartialPromised) {
		// Another order outranks the clear for the partial: the job cannot be
		// born, and the zero is still the station's fact.
		return e.clearZeroOnly(job)
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
		LoaderID: job.LoaderID, EmptyBin: empty, PartialAt: job.PartialAt.Name,
		Prepared: true, Pulls: pulls, Spot: spot})
	if err != nil || arm == "" {
		// The candidates declined the empty and no spot is open: the same gap,
		// reached after the birth. The job ends (nothing of it is left live) and
		// the zero stands.
		why := fmt.Sprintf("no buffer spot is open and no pull took %s", job.Empty.Label)
		if err != nil {
			why = fmt.Sprintf("the clear could not choose where %s goes: %v", job.Empty.Label, err)
		}
		d.EndClear(birth.Parent, why)
		return clearStays(job.Empty.ID, epoch)
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
// first buffer spot's in the loader's order — of the home's part, unclaimed.
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

// clearPartialOn is the unclaimed bin of the payload with parts left on spot.
func clearPartialOn(e *Engine, spot *nodes.Node, payload string) *bins.Bin {
	on, err := e.db.ListBinsByNode(spot.ID)
	if err != nil {
		return nil
	}
	for _, b := range on {
		if b.PayloadCode == payload && b.UOPRemaining > 0 && b.ClaimedBy == nil {
			return b
		}
	}
	return nil
}

// ClearPartialStays is what the station is told when a confirm records the zero
// and no job takes the empty: the two-robot trade's gap (STOP 6), the same
// sentence as the station's own tap says.
const ClearPartialStays = "the partial stays on its spot for now"

// clearStays is the success answer with no job: arm none, the zero carried.
func clearStays(binID, epoch int64) ClearHomeAnswer {
	return ClearHomeAnswer{Status: http.StatusOK, Body: protocol.ClearHomeResponse{
		Status: "ok", Arm: protocol.ClearArmNone, Detail: ClearPartialStays, BinID: binID, DeltaEpoch: epoch}}
}

// clearZeroOnly records the zero and births nothing: the carrier's clear (with
// its CMS departure if owed), in one transaction.
func (e *Engine) clearZeroOnly(job dispatch.ClearJob) ClearHomeAnswer {
	epoch, err := e.ClearForReuseAndBookDeparture(job.Empty.ID, job.Home.ID, nil)
	if err != nil {
		return clearRefused(http.StatusInternalServerError, "could not record %s empty: %v", job.Empty.Label, err)
	}
	return clearStays(job.Empty.ID, epoch)
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
