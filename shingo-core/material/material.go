package material

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"shingocore/store/bins"
	"shingocore/store/cms"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
)

// errCMSBoundaryCycle is returned by FindCMSBoundary when the parent
// chain revisits a node. It reaches the caller as an error and not as
// a nil node: a malformed tree is a failure to locate the boundary,
// which is a different answer from "this node has no boundary above
// it".
var errCMSBoundaryCycle = errors.New("cms boundary: parent chain cycle")

// MovementEvent is the minimal bin-movement payload the material
// package needs in order to log a movement between CMS boundaries.
// Mirrors the subset of engine.BinUpdatedEvent used by the old
// RecordMovementTransactions — BinID, FromNodeID, ToNodeID — so the
// engine wrapper can build one from its BinUpdatedEvent without
// pulling in the whole engine type.
type MovementEvent struct {
	BinID      int64
	FromNodeID int64
	ToNodeID   int64
	// RobotID and OrderID name who moved the bin. They arrive on the event
	// rather than being read from the bin, because the bin's claim is already
	// released by the time the event fires — reading bin.ClaimedBy here is what
	// made cms_transactions.order_id NULL for every ordinary delivery.
	RobotID string
	OrderID int64
}

// CMSStoreroomProperty is the node property that makes a node a CMS boundary
// and carries the storeroom code CMS knows it by ("SM01", "MAN", "DOCK").
//
// ONE PROPERTY, TWO ANSWERS, NO DEFAULT. Presence means "this is a boundary"
// and its value is where; absence means "not a boundary". A node is never a
// boundary because of where it sits in the tree.
const CMSStoreroomProperty = "cms_storeroom"

// CountCMSBoundaries returns how many nodes carry the cms_storeroom property.
//
// One home for the count, next to the property name it keys on. Two callers ask
// it — the startup warning and the health surface — and they were asking with
// two copies of the same SQL, which is one copy too many for a query whose
// answer decides whether a plant is reported as unconfigured or as broken.
//
// Zero is a legitimate state, not an error: it is what a plant looks like
// between configuring the endpoint and tagging the nodes. Both callers say so
// in their own words.
func CountCMSBoundaries(db *sql.DB) (int, error) {
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM node_properties WHERE key = $1`,
		CMSStoreroomProperty).Scan(&n); err != nil {
		return 0, fmt.Errorf("count cms boundaries: %w", err)
	}
	return n, nil
}

// FindCMSBoundary walks up the parent chain from nodeID and returns the nearest
// synthetic ancestor (or self) tagged with cms_storeroom, together with that
// storeroom's code.
//
// FAIL-CLOSED AT EVERY DEPTH. The predicate this replaces defaulted parentless
// synthetic nodes ON — enabled unless the property said "false" — and child
// synthetic nodes OFF. Nothing wrote that property in production, so the
// default was the whole behaviour, and it made every parentless synthetic node
// a CMS boundary: _TRANSIT, every node group, and every per-robot carrier node
// (_ROBOT:*). A bin picked up by a robot crossed from its real storeroom into
// "the robot", and shingo booked the transfer.
//
// Returns (nil, "", nil) when the walk reaches a root without finding a tagged
// ancestor. Returns (nil, "", err) when a Store call fails or the walk detects
// a cycle — "the lookup failed" is not "there is no boundary here", and a
// caller that cannot tell them apart emits zero transactions for a real move.
func FindCMSBoundary(s Store, nodeID int64) (*nodes.Node, string, error) {
	visited := make(map[int64]bool)
	currentID := nodeID
	for {
		if visited[currentID] {
			return nil, "", errCMSBoundaryCycle
		}
		visited[currentID] = true

		node, err := s.GetNode(currentID)
		if err != nil {
			return nil, "", err
		}

		if node.IsSynthetic {
			code, err := s.GetNodePropertyOrError(node.ID, CMSStoreroomProperty)
			if err != nil {
				return nil, "", err
			}
			if code != "" {
				return node, code, nil
			}
		}

		if node.ParentID == nil {
			return nil, "", nil
		}
		currentID = *node.ParentID
	}
}

// BuildMovementTransactions returns the CMS transaction rows that
// should be recorded when a bin moves between two nodes whose CMS
// boundaries differ. Negative deltas represent the bin leaving the
// source boundary; positive deltas represent arrival at the
// destination.
//
// Returns a nil slice when nothing needs to be recorded (source and
// destination resolve to the same boundary, the bin has no manifest
// items, or neither endpoint has a boundary). The caller should
// persist and emit for a non-nil slice; a nil slice and nil error
// means "no-op, carry on".
//
// The second return names the manifest lines that CROSSED A BOUNDARY AND COULD
// NOT BE COUNTED. It is separate from the error because those two are different
// answers: an error means the build could not be attempted, while an uncounted
// line means it was attempted and part of the answer is missing. Both are
// losses the caller must make loud; neither may be inferred from the row count,
// because a bin whose every line is uncountable returns the same empty slice as
// a move that legitimately crossed nothing.
func BuildMovementTransactions(s Store, ev MovementEvent) ([]*cms.Transaction, *UncountedLines, error) {
	// The storeroom code is stamped onto the row here, where the walk already
	// found it. Carrying it forward rather than re-deriving it downstream is
	// what lets the wire layer be a pure struct-to-struct map: a translator
	// that had to look up "which storeroom is node 41" would need the node tree
	// and a database, and would stop being testable as a table of inputs.
	var srcBoundary, dstBoundary *nodes.Node
	var srcStoreroom, dstStoreroom string
	if ev.FromNodeID != 0 {
		b, code, err := FindCMSBoundary(s, ev.FromNodeID)
		if err != nil {
			return nil, nil, err
		}
		srcBoundary, srcStoreroom = b, code
	}
	if ev.ToNodeID != 0 {
		b, code, err := FindCMSBoundary(s, ev.ToNodeID)
		if err != nil {
			return nil, nil, err
		}
		dstBoundary, dstStoreroom = b, code
	}

	srcID := int64(0)
	dstID := int64(0)
	if srcBoundary != nil {
		srcID = srcBoundary.ID
	}
	if dstBoundary != nil {
		dstID = dstBoundary.ID
	}
	if srcID == dstID {
		return nil, nil, nil
	}

	c, err := readBinContents(s, ev.BinID)
	if err != nil || c == nil {
		return nil, nil, err
	}

	// The order comes from the EVENT, not from bin.ClaimedBy. This used to read
	// the claim, and the claim is released by ApplyArrival before the event
	// fires — so order_id was NULL on every ordinary AMR delivery, a column
	// written by one path and true only on the paths nobody looked at.
	var orderID *int64
	if ev.OrderID != 0 {
		id := ev.OrderID
		orderID = &id
	}

	var txns []*cms.Transaction
	txns = append(txns, rowsAtBoundary(c, srcBoundary, srcStoreroom, -1, // leaving  → negative delta
		cms.SourceTypeMovement, orderID, ev.RobotID)...)
	txns = append(txns, rowsAtBoundary(c, dstBoundary, dstStoreroom, +1, // arriving → positive delta
		cms.SourceTypeMovement, orderID, ev.RobotID)...)

	report := c.report()
	if len(txns) == 0 {
		return nil, report, nil
	}
	return txns, report, nil
}

// ClearEvent is the minimal payload a CLEAR needs in order to book the
// departure of a bin's contents: which bin, and the node it is standing on.
//
// IT CARRIES NO ROBOT AND NO ORDER, and that is the difference from
// MovementEvent rather than an omission. A clear is a person at a station
// emptying a carrier — the unloader taking material out of the supermarket and
// into another CMS zone — so there is no AMR to name in Resource and no order
// to key it to. Inventing either would be a claim about the plant that is not
// true.
type ClearEvent struct {
	BinID  int64
	NodeID int64
}

// BuildClearTransactions returns the CMS transaction rows that should be
// recorded when the bin at nodeID is CLEARED — the unloader's door, where
// material leaves the supermarket for another CMS zone.
//
// DELIBERATELY ONE-SIDED, and that is the design rather than an unfinished
// half. shingo does not know which CMS zone the material went to, so it books
// only the departure it can see; CMS credits the destination by its own logic,
// as it credits MAN at label print. A guessed destination storeroom would be
// indistinguishable from a measured one once it reached the ledger.
//
// Returns a nil slice when nothing needs recording: the node resolves to no
// boundary (an untagged clear is invisible to CMS and that is correct), the bin
// is drained, or it carries no manifest and no template.
//
// THE BOUNDARY IS RESOLVED BEFORE THE BIN IS READ, which is what keeps an
// untagged site away from this code entirely. Nothing downstream — the manifest
// parse, the template lookup, the refusal on a line that names no part — can
// fail for a plant that has tagged nothing, because none of it runs. A site
// participates in CMS by tagging a node, and until it does, this function's only
// reachable answer is "nothing".
//
// The second return names the manifest lines that could not be counted, on the
// same terms as BuildMovementTransactions: separate from the error because an
// error means the build could not be attempted, while an uncounted line means
// it was attempted and part of the answer is missing. On this path the loss is
// worse than on a movement, because the clear destroys the contents the count
// would have been derived from — see the caller.
func BuildClearTransactions(s Store, ev ClearEvent) ([]*cms.Transaction, *UncountedLines, error) {
	boundary, storeroom, err := FindCMSBoundary(s, ev.NodeID)
	if err != nil {
		// THREE-VALUED, AND THE CALLER MUST NOT COLLAPSE IT. A failed lookup is
		// not "no boundary": read that way, a transient database error books
		// nothing for material that physically left the storeroom, and the
		// clear that destroys the evidence still succeeds.
		return nil, nil, err
	}
	if boundary == nil {
		return nil, nil, nil
	}

	c, err := readBinContents(s, ev.BinID)
	if err != nil || c == nil {
		return nil, nil, err
	}

	txns := rowsAtBoundary(c, boundary, storeroom, -1, cms.SourceTypeClear, nil, "")
	report := c.report()
	if len(txns) == 0 {
		return nil, report, nil
	}
	return txns, report, nil
}

// binContents is what a bin comes to at emission time: the carrier itself, the
// manifest lines it names, the template ratio each line is counted by, and the
// lines no ratio could count.
//
// ONE DERIVATION, TWO CALLERS. A movement and a clear ask the same question of
// a bin — what is in it, and how much of each — and differ only in which
// boundaries the answer is booked against and with what sign. A second copy of
// the quantity maths is a second place for it to drift from
// payload_manifest.parts_per_cycle.
type binContents struct {
	bin       *bins.Bin
	items     []bins.ManifestEntry
	perCycle  map[string]int64
	uncounted []string
}

// readBinContents resolves a bin and the template its counts derive from.
//
// (nil, nil) means there is nothing any boundary could book, for a reason that
// is not a failure: the bin has no manifest lines, or no template to count them
// by. Those are different from an error, which means the question could not be
// answered at all.
func readBinContents(s Store, binID int64) (*binContents, error) {
	bin, err := s.GetBin(binID)
	if err != nil {
		return nil, err
	}

	// An unparseable manifest is a failure to answer the question, not an
	// answer of "no parts". Discarding the error here reported every
	// corrupt manifest as an empty bin and emitted zero CMS rows for a
	// real physical move.
	parsed, err := bin.ParseManifest()
	if err != nil {
		return nil, err
	}
	if parsed == nil || len(parsed.Items) == 0 {
		return nil, nil
	}

	// THE COUNT IS DERIVED, NOT READ. The manifest lists which parts are in
	// the carrier; how many of each is uop_remaining x the template's
	// parts_per_cycle, computed here at emission. The bin manifest used to
	// carry a stored qty that no writer agreed on and nothing rewrote as
	// production drew the bin down, so whatever it held went stale on the
	// first consumed part.
	perCycle, err := postablePartsPerCycle(s, bin.PayloadCode)
	if err != nil {
		return nil, err
	}
	if perCycle == nil {
		// No template to count by. Skipping is deliberate: a movement row
		// with a guessed quantity is worse than no row, because it is
		// indistinguishable from a measured one once it reaches CMS.
		return nil, nil
	}

	// WHICH LINES THE TEMPLATE CANNOT COUNT, decided ONCE for the bin rather
	// than once per boundary. It is a property of the manifest and the
	// template, not of which way the bin crossed, and counting it per side
	// would report one unknown part twice whenever both endpoints are tagged.
	//
	// A DRAINED BIN IS NOT AN UNCOUNTABLE ONE. uop_remaining of zero (or the
	// negative an overpacked bin carries) makes every count non-positive for a
	// reason the template answered perfectly well, so it yields no rows and no
	// finding. Only a MISSING RATIO means the question went unanswered.
	var uncounted []string
	if bin.UOPRemaining > 0 {
		for _, m := range parsed.Items {
			if perCycle[m.PartNumber] <= 0 {
				uncounted = append(uncounted, m.PartNumber)
			}
		}
	}

	return &binContents{bin: bin, items: parsed.Items, perCycle: perCycle, uncounted: uncounted}, nil
}

// report names the uncounted lines for the caller's counter, or nil when every
// line that crossed was counted.
func (c *binContents) report() *UncountedLines {
	if len(c.uncounted) == 0 {
		return nil
	}
	return &UncountedLines{PayloadCode: c.bin.PayloadCode, CatIDs: c.uncounted}
}

// rowsAtBoundary turns a bin's countable lines into one transaction per line at
// boundary, signed by sign. A nil boundary yields nothing, so a caller with only
// one tagged endpoint passes the other one nil rather than branching.
func rowsAtBoundary(c *binContents, boundary *nodes.Node, storeroom string, sign int64,
	sourceType string, orderID *int64, robotID string) []*cms.Transaction {
	if boundary == nil {
		return nil
	}
	var txns []*cms.Transaction
	for _, m := range c.items {
		// The MAGNITUDE is tested, then the direction applied. Testing the
		// signed value instead would pass an overpacked bin's negative
		// remainder through the source side's -1 and book a positive
		// arrival where a part left.
		count := int64(c.bin.UOPRemaining) * c.perCycle[m.PartNumber]
		if count <= 0 {
			continue
		}
		txns = append(txns, &cms.Transaction{
			NodeID:      boundary.ID,
			NodeName:    boundary.Name,
			Storeroom:   storeroom,
			CatID:       m.PartNumber,
			Delta:       sign * count,
			BinID:       &c.bin.ID,
			BinLabel:    c.bin.Label,
			PayloadCode: c.bin.PayloadCode,
			SourceType:  sourceType,
			OrderID:     orderID,
			RobotID:     robotID,
		})
	}
	return txns
}

// UncountedLines names the manifest lines a movement could not turn into a
// count, and the payload whose template did not carry them.
//
// A RETURN VALUE RATHER THAN A LOG LINE, because the caller is the one holding
// the counter and this is a loss: the bin physically crossed a storeroom
// boundary carrying that part, and nothing will ever book it. Left to
// `continue` silently — which is what the code did — a partial-release bin
// whose manifest named the payload code rather than a part number booked
// exactly nothing, and every count on the health page reported a plant that had
// not moved anything.
//
// nil means every line that crossed was counted. A drained bin (uop_remaining
// of zero) is never reported: its counts are zero for a reason the template
// answered, which is a different thing from a question it could not answer.
type UncountedLines struct {
	PayloadCode string
	CatIDs      []string
}

// postablePartsPerCycle returns the payload template's per-cycle ratios, keyed
// by part number — and refuses when a line names no part.
//
// THE IDENTITY GATE, AND IT IS WHY A KIT CANNOT DOUBLE-BOOK. Every row a
// movement builds names a part on the wire, so every line it counts has to name
// one — and a line names a part exactly when it points at a parts row. An
// unresolved line still holds whatever was typed into a box labelled CATID, and
// posting that identifies the movement by a number the middleware has never
// seen.
//
// The failure this closes is arithmetic, not cosmetic. Before the parts table
// there was no way to tell a corrected line from an uncorrected one, so the
// interim fix bound the wire to the PAYLOAD CODE — right for a bin of one part,
// and for a kit it books the payload once per line: a payload-15 bin of
// capacity 1000 posts 1000 twice, 2,000 booked where 1,000 moved. Holding is
// the honest answer and it is cheap, because nothing has ever been posted from
// either plant; waiting for a person to name the kit's components loses nothing.
//
// nil, nil means "no template" — see templateLines for why that is not the same
// answer as an empty one.
func postablePartsPerCycle(s Store, payloadCode string) (map[string]int64, error) {
	template, err := templateLines(s, payloadCode)
	if err != nil || template == nil {
		return nil, err
	}
	if unresolved := unresolvedLines(template); len(unresolved) > 0 {
		return nil, fmt.Errorf("payload %s has %d manifest line(s) that name no part (%s): "+
			"a movement of it cannot be posted, because every line on the wire has to carry the "+
			"part number CMS books against. Enter the part on the payloads page",
			payloadCode, len(unresolved), strings.Join(unresolved, ", "))
	}
	perCycle := make(map[string]int64, len(template))
	for _, it := range template {
		perCycle[it.PartNumber] = it.PartsPerCycle
	}
	return perCycle, nil
}

// unresolvedLines names the template lines that point at no part. A line names
// a part exactly when it carries a part_id — the foreign key IS the test, which
// is the point of having one instead of a rule about what a string looks like.
func unresolvedLines(items []*payloads.ManifestItem) []string {
	var out []string
	for _, it := range items {
		if it.PartID == 0 {
			out = append(out, it.PartNumber)
		}
	}
	return out
}

// templateLines returns the payload template's manifest lines, or nil when the
// payload has no template.
//
// nil and an empty slice are DIFFERENT ANSWERS, and BuildMovementTransactions
// acts on the difference: nil means "there is no template, so no count can be
// derived" and it emits nothing at all, while an empty slice means "the
// template exists and lists no parts" — every manifest line is then uncountable
// and reported as such. Returning empty for both would turn a missing template
// into a silent nothing rather than a finding.
//
// A bin with no payload_code has no template by construction; that is a bare
// carrier, and it returns nil rather than an error.
func templateLines(s Store, payloadCode string) ([]*payloads.ManifestItem, error) {
	if payloadCode == "" {
		return nil, nil
	}
	p, err := s.GetPayloadByCode(payloadCode)
	if errors.Is(err, sql.ErrNoRows) {
		// Not found is not an error worth failing a movement over — Edge can
		// drive a direct load with a code Core has no template for. The
		// comment always said so; the code returned every error including this
		// one, so the sentence above it described a branch that did not exist.
		return nil, nil
	}
	if err != nil {
		// Anything else IS worth failing over, and that is the conservative
		// reading: an unreadable template must not silently become a
		// zero-quantity move.
		return nil, fmt.Errorf("get payload by code %q: %w", payloadCode, err)
	}
	items, err := s.ListPayloadManifest(p.ID)
	if err != nil {
		return nil, err
	}
	if items == nil {
		// A template with no lines still EXISTS, and the caller has to be able
		// to tell that from "no template". A nil slice from the store would
		// collapse the two.
		items = []*payloads.ManifestItem{}
	}
	return items, nil
}
