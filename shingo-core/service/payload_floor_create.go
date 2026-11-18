package service

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
	"unicode"

	"shingo/protocol"

	"shingocore/store"
	"shingocore/store/payloads"
)

// payload_floor_create.go — a part made new at a station's changeover picker
// (protocol.AskPartCreate). The floor create runs every check a desktop create
// runs (checkPayload, checkSets) and three of its own, writes the part with a
// record of who made it in one transaction, and answers a repeat of the same
// submit with the part it made the first time.

// FloorCreateAction is the audit_log action of a part made at the floor: its
// row (store.PayloadAuditEntity, the payload's id) holds the submit as Core
// read it (floorSubmit), and the station as its actor. Read by the repeat
// check (CreateFromFloor) and the payloads page (FloorMade).
const FloorCreateAction = "floor_create"

// ErrUnknownCarrierType refuses a floor part whose carrier type Core does not
// have.
var ErrUnknownCarrierType = errors.New("unknown carrier type")

// PartExistsError refuses a floor part Core already has. Code is Core's
// spelling; Near is true when the submitted number matches it only once case,
// spaces, hyphens and underscores are ignored (partSpelling).
type PartExistsError struct {
	Code string
	Near bool
}

func (e *PartExistsError) Error() string {
	if e.Near {
		return fmt.Sprintf("the part already exists as %s", e.Code)
	}
	return fmt.Sprintf("part %s already exists", e.Code)
}

// CATIDTakenError refuses a floor part whose CATID another part already
// carries: two parts on one CATID make their counts unattributable.
type CATIDTakenError struct {
	CATID      string
	PartNumber string
}

func (e *CATIDTakenError) Error() string {
	return fmt.Sprintf("cat id %s is already on part %s", e.CATID, e.PartNumber)
}

// floorSubmit is a floor create's record: the station that asked and the
// request as Core read it (the part number and the CATID trimmed, the carrier
// type by Core's code). It is the audit row's new_value, and a repeat is a
// submit whose record equals the stored one.
type floorSubmit struct {
	Station string `json:"station"`
	protocol.PartCreateRequest
}

// CreateFromFloor creates the part a station submitted at its changeover
// picker, or answers a repeat of that submit. The steps, in order:
//
//  1. the checks every create runs (checkPayload: the number trimmed and not
//     blank, the pack and the tote numbers 1 or more), the carrier type
//     resolved by its code (unknown refused, bare refused by checkSets), the
//     manifest one line, the part number at ratio 1;
//  2. the exact number exists: the same station and process submitting the
//     same fields get the stored part back, repeat true, and nothing is
//     written; anything else is refused (PartExistsError);
//  3. a near spelling exists (partSpelling): refused, naming Core's spelling;
//  4. a CATID another part carries (CATIDTakenError), or one that differs
//     from this part's known one (CATIDConflict): refused, not asked;
//  5. one transaction: the payload, its carrier, its manifest line and the
//     audit_log row (FloorCreateAction). A concurrent submit of the same
//     number that committed first is seen at the unique constraint, and the
//     submit goes back to step 2;
//  6. the payloads hook (payloadsWritten).
//
// The returned payload is the stored row; repeat is true when nothing was
// written.
func (s *PayloadService) CreateFromFloor(station string, req protocol.PartCreateRequest) (*payloads.Payload, bool, error) {
	p := &payloads.Payload{Code: req.PartNumber, UOPCapacity: req.StandardPack,
		ToteLayers: req.ToteLayers, UOPPerTote: req.UOPPerTote}
	if err := checkPayload(p); err != nil {
		return nil, false, err
	}
	carrier := strings.TrimSpace(req.BinTypeCode)
	bt, err := s.db.GetBinTypeByCode(carrier)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, fmt.Errorf("%w %q", ErrUnknownCarrierType, carrier)
	}
	if err != nil {
		return nil, false, err
	}
	catid := strings.TrimSpace(req.CATID)
	sets := PayloadSets{
		BinTypeIDs: &[]int64{bt.ID},
		Manifest:   &[]ManifestLine{{PartNumber: p.Code, CATID: catid, PartsPerCycle: 1}},
	}
	if err := s.checkSets(sets); err != nil {
		return nil, false, err
	}
	submit := floorSubmit{Station: station, PartCreateRequest: protocol.PartCreateRequest{
		ProcessID: req.ProcessID, PartNumber: p.Code, StandardPack: p.UOPCapacity, BinTypeCode: bt.Code,
		CATID: catid, ToteLayers: p.ToteLayers, UOPPerTote: p.UOPPerTote,
	}}
	record, err := json.Marshal(submit)
	if err != nil {
		return nil, false, err
	}

	// Two passes at most: the second is the concurrent submit's, which step 2
	// then sees committed.
	for range 2 {
		existing, err := s.db.GetPayloadByCode(p.Code)
		switch {
		case err == nil:
			return s.floorRepeat(existing, submit)
		case !errors.Is(err, sql.ErrNoRows):
			return nil, false, err
		}
		if err := s.refuseNearOrTaken(p.Code, catid); err != nil {
			return nil, false, err
		}
		row := *p
		err = s.db.CreatePayloadWithSetsAudited(&row, sets, FloorCreateAction, string(record), station)
		if errors.Is(err, payloads.ErrCodeTaken) {
			continue
		}
		if err := s.payloadsWritten(err); err != nil {
			return nil, false, err
		}
		return &row, false, nil
	}
	return nil, false, fmt.Errorf("part %s: the code was taken twice while it was being made", p.Code)
}

// catalogRowFunc reads one part's catalog row and carrier rows as the feeds
// build them (messaging's CoreDataService.CatalogRow).
type catalogRowFunc = func(code string) (protocol.CatalogPayloadInfo, []protocol.PayloadBinTypeInfo, error)

// SetCatalogRowFunc installs the read behind CatalogRow. Set once, through
// Engine.SetCatalogRowFunc.
func (s *PayloadService) SetCatalogRowFunc(fn catalogRowFunc) {
	s.catalogRow.Store(&fn)
}

// CatalogRow is one part's catalog row and its carrier rows, built by the
// builders the catalog feed and the node list use, for the floor create's
// answer: what the Edge writes from it is what the next feeds carry. An error
// when nothing is wired.
func (s *PayloadService) CatalogRow(code string) (protocol.CatalogPayloadInfo, []protocol.PayloadBinTypeInfo, error) {
	fn := s.catalogRow.Load()
	if fn == nil || *fn == nil {
		return protocol.CatalogPayloadInfo{}, nil, errors.New("the catalog read is not wired")
	}
	return (*fn)(code)
}

// floorRepeat answers a floor submit whose part number Core already has: the
// stored part, repeat true, when a floor create of it recorded this very
// submit (the same station, process and fields); otherwise PartExistsError.
func (s *PayloadService) floorRepeat(existing *payloads.Payload, submit floorSubmit) (*payloads.Payload, bool, error) {
	entries, err := s.db.ListEntityAudit(store.PayloadAuditEntity, existing.ID)
	if err != nil {
		return nil, false, fmt.Errorf("read who made part %s: %w", existing.Code, err)
	}
	for _, e := range entries {
		if e.Action != FloorCreateAction {
			continue
		}
		var made floorSubmit
		if err := json.Unmarshal([]byte(e.NewValue), &made); err != nil {
			continue
		}
		if reflect.DeepEqual(made, submit) {
			return existing, true, nil
		}
	}
	return nil, false, &PartExistsError{Code: existing.Code}
}

// FloorMade is who made a part at the floor, and when: the station, its
// process's id, and the time the create committed (the audit row's).
type FloorMade struct {
	Station   string
	ProcessID int64
	At        time.Time
}

// FloorMade reads, for every part made at the floor, who made it and when,
// keyed by payload id, from the audit rows its creates wrote
// (FloorCreateAction): one statement. The payloads page shows it on the
// part's row. A row whose record does not decode is left out.
func (s *PayloadService) FloorMade() (map[int64]*FloorMade, error) {
	entries, err := s.db.ListAuditByAction(store.PayloadAuditEntity, FloorCreateAction)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]*FloorMade, len(entries))
	for _, e := range entries {
		var made floorSubmit
		if err := json.Unmarshal([]byte(e.NewValue), &made); err != nil {
			continue
		}
		if _, seen := out[e.EntityID]; !seen {
			out[e.EntityID] = &FloorMade{Station: made.Station, ProcessID: made.ProcessID, At: e.CreatedAt}
		}
	}
	return out, nil
}

// refuseNearOrTaken is steps 3 and 4: a payload whose code is code spelled
// another way, a part other than code already carrying catid, and a known
// part code whose CATID differs from catid. One read of the payloads and one
// of the parts.
func (s *PayloadService) refuseNearOrTaken(code, catid string) error {
	all, err := s.db.ListPayloads()
	if err != nil {
		return err
	}
	spelling := partSpelling(code)
	for _, other := range all {
		if partSpelling(other.Code) == spelling {
			return &PartExistsError{Code: other.Code, Near: true}
		}
	}
	if catid == "" {
		return nil
	}
	parts, err := s.db.ListParts()
	if err != nil {
		return err
	}
	for _, pt := range parts {
		switch {
		case pt.PartNumber == code && pt.CATID != "" && pt.CATID != catid:
			return &CATIDConflict{PartNumber: code, Existing: pt.CATID, Incoming: catid}
		case pt.PartNumber != code && pt.CATID == catid:
			return &CATIDTakenError{CATID: catid, PartNumber: pt.PartNumber}
		}
	}
	return nil
}

// partSpelling is a part number with case, spaces, hyphens and underscores
// ignored: two numbers with one spelling are taken for one part typed two
// ways. The check is biased to refuse: a wrong refusal costs an engineer's
// visit, a missed one splits a part's stock across two codes.
func partSpelling(number string) string {
	var b strings.Builder
	for _, r := range number {
		if unicode.IsSpace(r) || r == '-' || r == '_' {
			continue
		}
		b.WriteRune(unicode.ToUpper(r))
	}
	return b.String()
}
