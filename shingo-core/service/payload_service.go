package service

import (
	"errors"
	"fmt"
	"strings"

	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
)

// PayloadService centralizes payload-template CRUD, manifest-item
// mutations, bin-type associations, and node-compatibility lookups.
// Handlers call PayloadService instead of reaching through engine
// passthroughs to *store.DB.
//
// Absorbed from engine_db_methods.go as part of the Phase 3a closeout
// (PR 3a.6). Methods are thin delegates today.
type PayloadService struct {
	db *store.DB
}

func NewPayloadService(db *store.DB) *PayloadService {
	return &PayloadService{db: db}
}

// --- Payload CRUD ---------------------------------------------------------

// Create inserts a new payload template.
func (s *PayloadService) Create(p *payloads.Payload) error {
	return s.db.CreatePayload(p)
}

// Get loads a payload template by ID.
func (s *PayloadService) Get(id int64) (*payloads.Payload, error) {
	return s.db.GetPayload(id)
}

// GetByCode loads a payload template by its catalogue code.
func (s *PayloadService) GetByCode(code string) (*payloads.Payload, error) {
	return s.db.GetPayloadByCode(code)
}

// Update persists field changes on a payload template.
func (s *PayloadService) Update(p *payloads.Payload) error {
	return s.db.UpdatePayload(p)
}

// Delete removes a payload template.
func (s *PayloadService) Delete(id int64) error {
	return s.db.DeletePayload(id)
}

// List returns every payload template.
func (s *PayloadService) List() ([]*payloads.Payload, error) {
	return s.db.ListPayloads()
}

// --- Manifest items -------------------------------------------------------

// ListManifest returns the manifest items defined for a payload
// template.
func (s *PayloadService) ListManifest(payloadID int64) ([]*payloads.ManifestItem, error) {
	return s.db.ListPayloadManifest(payloadID)
}

// ValidateManifestLines rejects a manifest line that names no part or whose
// per-cycle ratio is missing or non-positive, naming every offending line
// rather than the first.
//
// IT LIVES HERE SO EVERY DOOR INHERITS IT. It used to sit in the www package
// and be called by each handler that remembered to, which is the arrangement
// that let apiSavePayloadManifestTemplate reach this column with no validation
// at all. A rule enforced by five callers is a rule with five chances to be
// forgotten; enforced by the write path, it has none.
//
// A MISSING RATIO ARRIVES AS ZERO AND CANNOT BE TOLD FROM A DECLARED ONE. JSON
// omits it, a blank browser box submits as 0, and an empty spreadsheet cell
// parses to 0 — three spellings of "I did not say" landing on a value that
// reads as "there are none of these in the bin". Four rows reached Springfield
// that way (payload_manifest ids 117/123/137/150) and were corrected by hand at
// the plant once the owner declared them entry oversights.
//
// Non-positive is refused rather than only missing, because the count a bin
// ships to the inventory ledger is uop_remaining x this number: a zero line
// contributes nothing to any count while looking configured. A part that
// genuinely is not in the carrier is a line that does not belong on the
// manifest.
//
// A BLANK PART NUMBER IS REFUSED, NOT SKIPPED. The old version continued past
// one, so a line with no part and a zero ratio passed validation and was then
// written — the blank-line `continue` that made the ratio check optional for
// exactly the lines least likely to be deliberate.
// ManifestLine is what a caller states about one line of a payload's manifest:
// which part, that part's controls identity, and the per-cycle ratio. Aliased
// here because a handler is not allowed to reach into store/ for it and should
// not have to — this is the service's own request shape.
type ManifestLine = payloads.Line

// CATIDConflict is raised when a caller supplies a cat id for a part that
// already carries a different one. Aliased for the same reason as ManifestLine:
// a handler has to be able to recognise it to answer 409 rather than 500.
type CATIDConflict = payloads.ErrCATIDConflict

// ErrManifestLine marks a validation failure so a handler can render it as the
// caller's error rather than the server's, without matching on message text.
var ErrManifestLine = errors.New("manifest line")

func ValidateManifestLines(lines []payloads.Line) error {
	var bad []string
	for i, l := range lines {
		switch {
		case strings.TrimSpace(l.PartNumber) == "":
			bad = append(bad, fmt.Sprintf("line %d names no part", i+1))
		case l.PartsPerCycle < 1:
			bad = append(bad, fmt.Sprintf("%s (%d)", l.PartNumber, l.PartsPerCycle))
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("%w: every manifest line needs a part number and a parts_per_cycle of 1 "+
		"or more — how many of the part ONE production cycle uses, usually 1. Fix: %s",
		ErrManifestLine, strings.Join(bad, ", "))
}

// CreateManifestItem inserts a manifest item on a payload template, minting or
// matching the part it names. catid is the part's controls identity when the
// caller is originating it; blank leaves a known part's alone.
func (s *PayloadService) CreateManifestItem(item *payloads.ManifestItem, catid string) error {
	if err := ValidateManifestLines([]payloads.Line{{
		PartNumber: item.PartNumber, PartsPerCycle: item.PartsPerCycle,
	}}); err != nil {
		return err
	}
	return s.db.CreatePayloadManifestItem(item, catid)
}

// UpdateManifestItem adjusts a manifest item's part, that part's cat id, and
// its per-cycle ratio.
func (s *PayloadService) UpdateManifestItem(id int64, partNumber, catid string, partsPerCycle int64) error {
	if err := ValidateManifestLines([]payloads.Line{{
		PartNumber: partNumber, PartsPerCycle: partsPerCycle,
	}}); err != nil {
		return err
	}
	return s.db.UpdatePayloadManifestItem(id, partNumber, catid, partsPerCycle)
}

// DeleteManifestItem removes a manifest item from a payload template.
func (s *PayloadService) DeleteManifestItem(id int64) error {
	return s.db.DeletePayloadManifestItem(id)
}

// ReplaceManifest swaps out a payload template's whole manifest in one pass,
// validating every line and minting or matching each line's part.
func (s *PayloadService) ReplaceManifest(payloadID int64, lines []payloads.Line) error {
	if err := ValidateManifestLines(lines); err != nil {
		return err
	}
	return s.db.ReplacePayloadManifest(payloadID, lines)
}

// --- parts ----------------------------------------------------------------

// ListParts returns every part, ordered by number. The payloads page reads it
// to auto-fill a known part's cat id as the operator types.
func (s *PayloadService) ListParts() ([]*payloads.Part, error) { return s.db.ListParts() }

// GetPart returns one part by number, or sql.ErrNoRows when it is new.
func (s *PayloadService) GetPart(partNumber string) (*payloads.Part, error) {
	return s.db.GetPartByNumber(partNumber)
}

// SetPartCATID records a part's controls identity, overwriting whatever was
// there. This is the "yes, same part, the new one is right" answer to the
// conflict prompt — it is deliberately a separate, explicit call rather than
// something an ordinary manifest save can do by accident.
func (s *PayloadService) SetPartCATID(partNumber, catid string) error {
	return s.db.SetPartCATID(partNumber, catid)
}

// --- Bin-type associations ------------------------------------------------

// SetBinTypes replaces the set of compatible bin types for a payload
// template.
func (s *PayloadService) SetBinTypes(payloadID int64, binTypeIDs []int64) error {
	return s.db.SetPayloadBinTypes(payloadID, binTypeIDs)
}

// ListBinTypes returns the bin types compatible with the given
// payload template.
func (s *PayloadService) ListBinTypes(payloadID int64) ([]*bins.BinType, error) {
	return s.db.ListBinTypesForPayload(payloadID)
}

// --- Advanced load sequences ----------------------------------------------

// ListLoadSequenceNames returns every registered advanced-load-sequence name,
// for the payload-editor dropdown. The empty ("normal load") option is added by
// the UI, not stored.
func (s *PayloadService) ListLoadSequenceNames() ([]string, error) {
	return s.db.ListLoadSequenceNames()
}

// --- Node compatibility ---------------------------------------------------

// ListCompatibleNodes returns the nodes that accept the given payload
// template (via explicit assignment or inherited-all mode).
func (s *PayloadService) ListCompatibleNodes(payloadID int64) ([]*nodes.Node, error) {
	return s.db.ListNodesForPayload(payloadID)
}
