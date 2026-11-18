package engine

import (
	"database/sql"
	"fmt"
	"log"
	"strings"

	"shingo/protocol"
	"shingoedge/store/catalog"
	"shingoedge/store/processes"
)

// part_create.go — a part Core does not have, made at a station's changeover
// picker (protocol.AskPartCreate): the Edge's half of the door, and the
// answer's applier.
//
// CORE MAKES THE PART; THE EDGE ONLY ASKS AND APPLIES. The Edge refuses what
// it can see is wrong before anything is sent (checkNewPart); Core runs the
// desktop's checks and its own, writes the part, and answers with the part's
// catalog row and carrier rows as its feeds build them. The answer is applied
// by its kind (applyPartCreate), waiter or none, so a tap that stopped
// waiting, or an Edge that restarted, still gets the part on its picker.

// PartCreateInvalid is a new part the Edge refused before anything was sent:
// the sentence says what the form must change. Nothing was asked of Core.
type PartCreateInvalid struct{ Sentence string }

func (e *PartCreateInvalid) Error() string { return e.Sentence }

// CreatePart asks Core to make a new part for processID, submitted at one of
// its stations' changeover pickers. Refused before anything is sent: a process
// whose HMI setup is off (ErrFlowComposerDisabled, the mint's gate), a request
// checkNewPart refuses (*PartCreateInvalid), and a Core whose last heartbeat
// ack does not name the kind (doesNotAnswer). Otherwise the ask goes out and:
//
//   - Core made it, or this cell had already made it (Repeat): the answer, its
//     part already in the catalog and on the process's part set
//     (applyPartCreate runs before the waiter wakes);
//   - Core refused: a *CoreRefusal with Core's sentence; nothing applied;
//   - no answer within AskWait: ErrAskPending. The answer is applied when it
//     comes.
func (e *Engine) CreatePart(processID int64, req protocol.PartCreateRequest) (*protocol.PartCreateResponse, error) {
	process, err := e.db.GetProcess(processID)
	if err != nil {
		return nil, err
	}
	if !process.FlowComposerEnabled {
		return nil, ErrFlowComposerDisabled
	}
	req.ProcessID = processID
	if err := checkNewPart(&req, e.BinTypeFamilies()); err != nil {
		return nil, err
	}
	return askOrRefuse[protocol.PartCreateResponse](e, protocol.AskPartCreate, req)
}

// checkNewPart trims the request's words and refuses what the form cannot have
// meant: no part number, a standard pack below 1, no carrier or one Core did
// not send, a tote number below 1. families is the carrier types Core sent
// with its node list (BinTypeFamilies), which are every type but the bare
// markers; none heard refuses every carrier. Core checks all of it again.
func checkNewPart(req *protocol.PartCreateRequest, families map[string]string) error {
	req.PartNumber = strings.TrimSpace(req.PartNumber)
	req.BinTypeCode = strings.TrimSpace(req.BinTypeCode)
	req.CATID = strings.TrimSpace(req.CATID)
	refuse := func(sentence string) error { return &PartCreateInvalid{Sentence: sentence} }
	_, known := families[req.BinTypeCode]
	switch {
	case req.PartNumber == "":
		// OWNER-WORDS
		return refuse("Type the part number.")
	case req.StandardPack < 1:
		// OWNER-WORDS
		return refuse("The standard pack is the parts in a full bin: a whole number, 1 or more.")
	case req.BinTypeCode == "":
		// OWNER-WORDS
		return refuse("Choose the carrier the part rides in.")
	case !known:
		// OWNER-WORDS
		return refuse(fmt.Sprintf("Core has not sent a carrier type called %s. Choose one from the list.", req.BinTypeCode))
	case req.ToteLayers != nil && *req.ToteLayers < 1:
		// OWNER-WORDS
		return refuse("Tote layers is a whole number, 1 or more, or blank.")
	case req.UOPPerTote != nil && *req.UOPPerTote < 1:
		// OWNER-WORDS
		return refuse("Parts per tote is a whole number, 1 or more, or blank.")
	}
	return nil
}

// applyPartCreate is the part.create answer's applier, run on arrival whether
// or not the tap still waits (HandleEdgeAnswer): a yes, made or a repeat, puts
// the part on this Edge (applyNewPart); a refusal applies nothing.
func applyPartCreate(e *Engine, a *protocol.EdgeAnswer) {
	r, err := askInto[protocol.PartCreateResponse](a)
	if err != nil {
		log.Printf("new part: Core refused it: %v — nothing applied", err)
		return
	}
	if err := e.applyNewPart(r); err != nil {
		log.Printf("new part %q for process %d: %v — nothing applied", r.Payload.Code, r.ProcessID, err)
		return
	}
	log.Printf("new part %q for process %d applied (repeat %v)", r.Payload.Code, r.ProcessID, r.Repeat)
}

// applyNewPart writes Core's yes. ONE TRANSACTION: the part's catalog row (the
// catalog feed's conditional upsert, catalog.UpsertCatalogWith) and its row on
// the answer's process's part set (processes.AddProcessPayload, written only
// while that process exists), both or neither. Then the carrier rows join the
// held part-to-carrier rule (addPayloadBinTypes), and the boards are nudged.
//
// IT HOLDS NO FEED DIGEST. The catalog and node digests stay what arrived
// with the last feeds: Core's next ack names its new digests, and the feeds it
// sends rewrite these same rows to nothing new (catalogEntryOf). Holding a
// digest from here could quote one Core's memo does not hold yet, and the
// older catalog Core sent back against it would prune the row.
//
// IDEMPOTENT: a second apply finds the row unchanged (the upsert writes
// nothing), the part already on the set (INSERT OR IGNORE) and the carrier
// rows already held.
func (e *Engine) applyNewPart(r *protocol.PartCreateResponse) error {
	if r.Payload.ID == 0 || r.Payload.Code == "" {
		return fmt.Errorf("the answer names no part")
	}
	if err := catalog.UpsertCatalogWith(e.db.DB, catalogEntryOf(r.Payload), func(tx *sql.Tx) error {
		return processes.AddProcessPayload(tx, r.ProcessID, r.Payload.Code)
	}); err != nil {
		return err
	}
	e.addPayloadBinTypes(r.BinTypes)
	// The part rows and a tile's carriers read what was just written, and
	// nothing else says it moved.
	e.nudgeBoards()
	return nil
}
