package domain

import (
	"shingo/protocol"
)

// ── THE FLOW ─────────────────────────────────────────────────────────────────
//
// A flow is what the HMI composer authors: one cell per position, each saying
// which part is there, how it swaps, which position it pairs with, where bins
// come from, wait, and go, and which way the robot drives. It is the
// style_node_claims row seen from the floor — the columns an operator can
// reason about on a picture of the press, and none of the ones they cannot.
//
// A FlowCell's JSON keys are the ones domain.ValidateFlowPreset reads
// (core_node_name, paired_core_node, second_paired_core_node, inbound_source,
// outbound_destination, changeover_evac_destination, inbound_staging,
// outbound_staging, changeover_evac_nodes[]) plus role, swap_mode,
// payload_code, key_route and locked — so a preset is a flow with the parts
// blank, and the two shapes cannot drift.
//
// Collapse and Expand translate between a cell and a claim. The law that
// makes the composer safe to point at a real plant row is that the
// translation loses nothing: Expand(Collapse(c), &c) writes c back on every
// column the INSERT writes, and Collapse reads the same cell out of what Expand
// wrote. The columns a cell does not carry are not lost either — Expand copies
// the unconditional ones from the prior claim and leaves the pointer-gated ones
// nil — so a save cannot flatten a setting the composer never showed.

// FlowCell is one position of a flow.
type FlowCell struct {
	CoreNodeName string             `json:"core_node_name"`
	Role         protocol.ClaimRole `json:"role"`
	SwapMode     protocol.SwapMode  `json:"swap_mode"`
	// PayloadCode is the part. Blank in a preset, never blank in a saved flow.
	PayloadCode string `json:"payload_code"`
	// THE PARTNER AND ROUTING FIELDS ARE OMITTED WHEN BLANK, and the four
	// above are not. A cell always has an identity — the position, its role,
	// its choreography and its part — and a reader that had to test for those
	// would be reading something that is not a cell. These seven are a cell's
	// OPTIONAL furniture, blank on most positions of most flows, and spelling
	// them out cost 103 bytes of the 332 a Hopkinsville cell serialises to:
	// a third of every cell, on 240 cells, on a payload the HMI parses.
	//
	// SAFE BECAUSE BOTH SIDES ALREADY NORMALISE. composer-model.js reads every
	// one of them as `c.field || ''` (init, :497-505) so a missing key and an
	// empty string were always the same value to it, and it BUILDS the save
	// body from its own state rather than echoing this one, so the request
	// shape does not change. Expand maps a blank cell field to a blank column
	// either way.
	PairedCoreNode       string `json:"paired_core_node,omitempty"`
	SecondPairedCoreNode string `json:"second_paired_core_node,omitempty"`
	InboundSource        string `json:"inbound_source,omitempty"`
	InboundStaging       string `json:"inbound_staging,omitempty"`
	OutboundStaging      string `json:"outbound_staging,omitempty"`
	OutboundDestination  string `json:"outbound_destination,omitempty"`
	// ChangeoverEvacDestination is where a tooling evacuation sends this
	// cell's bins; blank falls back to OutboundDestination.
	ChangeoverEvacDestination string `json:"changeover_evac_destination,omitempty"`
	// ChangeoverEvacNodes marks which of this cell's positions hold bins that
	// are in the way of a setup.
	ChangeoverEvacNodes []string `json:"changeover_evac_nodes,omitempty"`
	// KeyRoute is "robot drives via": ordered waypoints, validated against the
	// vendor map, never against the node list.
	KeyRoute []string `json:"key_route,omitempty"`
	// Advanced is the desktop's Advanced modal (U9b): the columns the picture
	// has no line for, carried on the position they belong to.
	//
	// NIL MEANS "NO OPINION", and it is the standing case. Expand's
	// carry-through above is the right answer for a column the composer does
	// not show, and it stays the answer whenever this is nil — every save from
	// the station HMI, and every save from the desktop where nobody opened the
	// modal. A non-nil Advanced is an engineer having looked at these fields
	// and pressed Apply, and then it is the draft that is authoritative,
	// because carry-through reads the stored row and a draft change is by
	// definition not in it yet.
	//
	// Collapse deliberately does NOT fill it. A collapsed cell is the shape
	// flow_presets validates, and a preset carries a flow, not a policy; the
	// desktop reads what to show in the modal from ComposerStyle.Advanced,
	// beside the cells rather than inside them.
	Advanced *FlowAdvanced `json:"advanced,omitempty"`
}

// FlowAdvanced is the thirteen columns of a claim that the flow does not draw
// (spec §2 D2, ruling R2: this is the ONLY place they appear).
//
// The grouping is the modal's six sections, in order: part identity,
// replenishment, changeover specials, press hardware, station policy, board.
// Two of D2's fields are not here — evacuation positions and evacuation
// destination are on the cell already, because the picture draws them.
//
// Every field is a plain value, and all of them are written together: the
// modal shows what the row holds and Apply hands the whole section back, so
// there is no per-field "untouched" to model inside it. Untouched is the
// pointer on FlowCell.
// A ZERO IS THE DEFAULT, AND IS NOT SENT. Ten of these fields carry omitempty
// and three deliberately do not.
//
// The desktop read carries an Advanced block per position per style — 240 of
// them on the S0 fixture — and every one spelled out all fourteen fields even
// when all fourteen were the zero value. The model already reads it the other
// way: advancedFor merges key by key over ADVANCED_DEFAULTS and skips what is
// absent or null ("Absent and null both mean the default"), and each of the
// ten zero values IS its default there.
//
// The three that stay: allowed_payload_codes, because the modal is allowed to
// clear the list and an empty list has to be sayable; reorder_point_source,
// because Go's zero is "" and the model's default is "legacy" — omitting it
// would change the pill the modal draws; and the carryover disposition,
// because blank means replace and a reader should be told which.
//
// Unmarshalling is untouched — omitempty is a marshalling tag — so a save
// sending every key still writes every column.
type FlowAdvanced struct {
	// AllowedPayloadCodes is which payloads a robot may bring to this
	// position. Set explicitly here, it beats Expand's degenerate-list guess.
	AllowedPayloadCodes []string `json:"allowed_payload_codes"`
	// ReorderPoint / ReorderPointSource / AutoReorder are the replenishment
	// policy. The source is what the calculator stamps ("manual",
	// "calculated", "legacy"); the modal shows it as a pill beside the number.
	ReorderPoint          int    `json:"reorder_point,omitempty"`
	ReorderPointSource    string `json:"reorder_point_source"`
	AutoReorder           bool   `json:"auto_reorder,omitempty"`
	LinesideSoftThreshold int    `json:"lineside_soft_threshold,omitempty"`
	// AutoRequestPayload is a PAYLOAD CODE, not a flag: it names which payload
	// a vacated position asks for. Blank is off.
	AutoRequestPayload string `json:"auto_request_payload,omitempty"`
	AutoPush           bool   `json:"auto_push,omitempty"`
	// EvacuateOnChangeover and CarryoverDisposition are the changeover
	// specials that are not positions on the picture.
	//
	// KeepStaged IS NOT HERE, and the column it would write still is. flowspec
	// marks keep_staged Unused in every (role, mode) and every door refuses a
	// claim that asks for it (domain.KeepStagedWithheld) — nothing restages the
	// spare bin yet — so the block carried a field the modal never drew, the
	// validator never accepted and Expand could only ever write as false.
	// Counted at both plants on 2026-09-13 before it went: `SELECT COUNT(*)
	// FROM style_node_claims WHERE keep_staged = 1` is 0 at Springfield and 0
	// at Hopkinsville, so no stored row loses an opinion by the composer no
	// longer speaking the column. Expand leaves it alone now, which is what
	// NodeClaimInput's nil pointer means, so the column survives a save
	// untouched — pinned in flow_advanced_test.go.
	EvacuateOnChangeover bool                 `json:"evacuate_on_changeover,omitempty"`
	CarryoverDisposition CarryoverDisposition `json:"changeover_carryover_disposition"`
	// IndexRobotSupplies describes the cell's hardware, so the modal's note
	// says it is the same for every part on this press.
	IndexRobotSupplies bool `json:"index_robot_supplies,omitempty"`
	// AutoConfirm is station policy; the plant setting applies unless set.
	AutoConfirm bool `json:"auto_confirm,omitempty"`
	// Sequence is the order the loader board draws this position in. It is
	// not choreography and the picture has no line for it, so it is here —
	// under its own one-field Board section — rather than nowhere, which is
	// where it was (owner ruling 2026-09-10: R2's list grows by one).
	//
	// ZERO IS "THE STORE'S ORDER", not position zero: Expand speaks the field
	// only when the cell carries an Advanced at all, so the store's own
	// next-free-slot assignment survives every save nobody opened the sheet on.
	Sequence int `json:"sequence,omitempty"`
}

// AdvancedOf is the claim's advanced columns as the modal shows them. Nil in,
// nil out — "+ Add a position" has no prior and the modal opens on the INSERT
// defaults, which is the zero value with one exception the store shares: a
// blank carryover disposition reads as replace.
func AdvancedOf(c *NodeClaim) *FlowAdvanced {
	if c == nil {
		return nil
	}
	a := &FlowAdvanced{
		AllowedPayloadCodes:   cloneStrings(c.AllowedPayloadCodes),
		ReorderPoint:          c.ReorderPoint,
		ReorderPointSource:    c.ReorderPointSource,
		AutoReorder:           c.AutoReorder,
		LinesideSoftThreshold: c.LinesideSoftThreshold,
		AutoRequestPayload:    c.AutoRequestPayload,
		AutoPush:              c.AutoPush,
		EvacuateOnChangeover:  c.EvacuateOnChangeover,
		CarryoverDisposition:  CarryoverFor(c),
		IndexRobotSupplies:    c.IndexRobotSupplies,
		AutoConfirm:           c.AutoConfirm,
		Sequence:              c.Sequence,
	}
	return a
}

// Flow is the composer's whole picture of one style.
type Flow struct {
	Cells []FlowCell `json:"cells"`
}

// THE CELL IS NOT LOCKABLE, AND THERE IS NO LOCK.
//
// LockingFields used to name the columns of a claim that the composer does not
// author, and a cell whose claim carried any of them collapsed Locked: SaveFlow
// then refused to write it, so an operator could not flatten an engineer's
// reorder point by re-routing a node.
//
// It was belt-and-braces over a guarantee Expand already provides — the
// unspoken columns are copied from the prior claim, and the pointer-gated ones
// are left nil so the update does not touch them — and the price was that
// almost no existing cell could be edited at all: uop_capacity alone locked
// every live claim at both plants, and reorder_point locked nineteen more at
// Springfield. Ruled 2026-09-09: the guarantee stands on its own, pinned
// directly on the plant rows by
// TestFlowCarryThrough_UnlockedSaveLeavesEveryUnspokenColumnAlone in
// store/, and the composer's surface is bounded by what a cell carries rather
// than by a refusal.
//
// What replaces it is explicit and per process, not implicit and per cell:
// flow_composer_enabled says whether this process's operator may change the
// flow at all.

// Collapse is the cell a claim reads as. Pure.
func Collapse(c NodeClaim) FlowCell {
	return FlowCell{
		CoreNodeName:              c.CoreNodeName,
		Role:                      c.Role,
		SwapMode:                  c.SwapMode,
		PayloadCode:               c.PayloadCode,
		PairedCoreNode:            c.PairedCoreNode,
		SecondPairedCoreNode:      c.SecondPairedCoreNode,
		InboundSource:             c.InboundSource,
		InboundStaging:            c.InboundStaging,
		OutboundStaging:           c.OutboundStaging,
		OutboundDestination:       c.OutboundDestination,
		ChangeoverEvacDestination: c.ChangeoverEvacDestination,
		ChangeoverEvacNodes:       cloneStrings(c.ChangeoverEvacNodes),
		KeyRoute:                  cloneStrings(c.KeyRoute),
	}
}

// Expand is the write for a cell: the NodeClaimInput UpsertClaim takes.
//
// It honours updateClaim's boundary. Of the eighteen columns the store writes
// unconditionally, the cell supplies role, swap_mode, payload_code, the two
// paired positions and the four routing fields; the other nine (uop_capacity,
// reorder_point, allowed_payload_codes, auto_request_payload,
// evacuate_on_changeover, auto_confirm, lineside_soft_threshold,
// reuse_compatible_bins, auto_push) are copied from the prior claim — or take
// the INSERT defaults with no prior — because a value type cannot say "no
// opinion" and the composer has none. Of the twelve pointer-gated columns it
// speaks exactly the three the cell carries (changeover_evac_nodes,
// changeover_evac_destination, key_route) and leaves the other nine nil, so an
// update leaves them alone.
//
// StyleID comes from the prior; a caller writing a new claim sets it. The
// prior's degenerate allowed list — exactly its one payload — follows the
// cell's payload rather than pinning the old part.
//
// All of that describes a cell whose Advanced is nil, which is every save the
// station HMI makes and every desktop save nobody opened the modal on. A cell
// that DOES carry Advanced overrides seven of the nine carried columns and
// five of the nine silent ones, at the bottom of the function — see FlowCell.
func Expand(cell FlowCell, prior *NodeClaim, source, calledBy string) NodeClaimInput {
	in := NodeClaimInput{
		CoreNodeName:              cell.CoreNodeName,
		Role:                      cell.Role,
		SwapMode:                  cell.SwapMode,
		PayloadCode:               cell.PayloadCode,
		PairedCoreNode:            cell.PairedCoreNode,
		SecondPairedCoreNode:      cell.SecondPairedCoreNode,
		InboundSource:             cell.InboundSource,
		InboundStaging:            cell.InboundStaging,
		OutboundStaging:           cell.OutboundStaging,
		OutboundDestination:       cell.OutboundDestination,
		ChangeoverEvacDestination: Ptr(cell.ChangeoverEvacDestination),
		ChangeoverEvacNodes:       Ptr(nonNilStrings(cell.ChangeoverEvacNodes)),
		KeyRoute:                  Ptr(nonNilStrings(cell.KeyRoute)),
		Source:                    source,
		CalledBy:                  calledBy,
	}
	if prior != nil {
		in.StyleID = prior.StyleID
		in.ReorderPoint = prior.ReorderPoint
		in.AllowedPayloadCodes = cloneStrings(prior.AllowedPayloadCodes)
		if len(prior.AllowedPayloadCodes) == 1 && prior.AllowedPayloadCodes[0] == prior.PayloadCode {
			in.AllowedPayloadCodes = []string{cell.PayloadCode}
		}
		in.AutoRequestPayload = prior.AutoRequestPayload
		in.EvacuateOnChangeover = prior.EvacuateOnChangeover
		in.AutoConfirm = prior.AutoConfirm
		in.LinesideSoftThreshold = prior.LinesideSoftThreshold
		in.ReuseCompatibleBins = prior.ReuseCompatibleBins
		in.AutoPush = prior.AutoPush
	}
	// The Advanced modal, LAST, so an engineer's opinion beats both the
	// carry-through above and the degenerate-list guess inside it. Nil is the
	// standing case and leaves everything above exactly as it was — which is
	// the whole reason the field is a pointer.
	//
	// Six of the thirteen are pointer-gated columns Expand otherwise leaves
	// nil; speaking them is what makes an update touch them at all. The other
	// seven are unconditional columns that were being copied from the prior a
	// moment ago and are now the modal's.
	if a := cell.Advanced; a != nil {
		in.AllowedPayloadCodes = cloneStrings(a.AllowedPayloadCodes)
		in.ReorderPoint = a.ReorderPoint
		in.LinesideSoftThreshold = a.LinesideSoftThreshold
		in.AutoRequestPayload = a.AutoRequestPayload
		in.AutoPush = a.AutoPush
		in.EvacuateOnChangeover = a.EvacuateOnChangeover
		in.AutoConfirm = a.AutoConfirm
		in.ReorderPointSource = Ptr(a.ReorderPointSource)
		in.AutoReorder = Ptr(a.AutoReorder)
		in.IndexRobotSupplies = Ptr(a.IndexRobotSupplies)
		in.ChangeoverCarryoverDisposition = Ptr(a.CarryoverDisposition)
		in.Sequence = Ptr(a.Sequence)
	}
	return in
}

// InputFromClaim is the write that rewrites a stored claim as it is: every
// column spoken, the pointer-gated ones included. It is how a locked cell is
// re-stamped and how a stored claim is put in front of the validator.
func InputFromClaim(c NodeClaim) NodeClaimInput {
	return NodeClaimInput{
		StyleID:                        c.StyleID,
		CoreNodeName:                   c.CoreNodeName,
		Role:                           c.Role,
		SwapMode:                       c.SwapMode,
		PayloadCode:                    c.PayloadCode,
		ReorderPoint:                   c.ReorderPoint,
		InboundStaging:                 c.InboundStaging,
		OutboundStaging:                c.OutboundStaging,
		InboundSource:                  c.InboundSource,
		OutboundDestination:            c.OutboundDestination,
		AllowedPayloadCodes:            cloneStrings(c.AllowedPayloadCodes),
		AutoRequestPayload:             c.AutoRequestPayload,
		EvacuateOnChangeover:           c.EvacuateOnChangeover,
		PairedCoreNode:                 c.PairedCoreNode,
		SecondPairedCoreNode:           c.SecondPairedCoreNode,
		ChangeoverEvacNodes:            Ptr(nonNilStrings(c.ChangeoverEvacNodes)),
		ChangeoverEvacDestination:      Ptr(c.ChangeoverEvacDestination),
		ChangeoverCarryoverDisposition: Ptr(c.ChangeoverCarryoverDisposition),
		IndexRobotSupplies:             Ptr(c.IndexRobotSupplies),
		KeyRoute:                       Ptr(nonNilStrings(c.KeyRoute)),
		KeyTask:                        Ptr(c.KeyTask),
		AutoConfirm:                    c.AutoConfirm,
		LinesideSoftThreshold:          c.LinesideSoftThreshold,
		ReuseCompatibleBins:            c.ReuseCompatibleBins,
		AutoPush:                       c.AutoPush,
		ReorderPointSource:             Ptr(c.ReorderPointSource),
		AutoReorder:                    Ptr(c.AutoReorder),
		KeepStaged:                     Ptr(c.KeepStaged),
		Sequence:                       Ptr(c.Sequence),
		Source:                         c.Source,
		CalledBy:                       c.CalledBy,
		SourcePresetID:                 clonePtr(c.SourcePresetID),
		SourcePresetVersion:            clonePtr(c.SourcePresetVersion),
	}
}

// InputFromClaimUngated is InputFromClaim with every absent-means-untouched
// column left ABSENT: the write a caller makes when it edits a few columns of
// a stored claim and has no opinion about the rest.
//
// WHY IT IS NOT JUST "WRITE THE THREE COLUMNS". NodeClaimInput has two kinds
// of field. The pointer-gated ones are absent-means-untouched, so a partial
// caller simply leaves them nil. The rest are UNCONDITIONAL: an empty value is
// written as empty, so a caller that names only its three columns blanks the
// claim's part, its source and its destination. A partial write therefore has
// to echo every unconditional column — and the echo is a list, and a list is a
// thing to forget.
//
// It was forgotten. The replenishment page's reorder edit kept its own copy of
// that list; five pointer-gated columns arrived after it was written, nobody
// added them, and the copy that DID echo four of them wiped a press's
// evacuation positions, its evacuation destination, its loader card and its
// key route on a reorder-point change. Deriving the echo from InputFromClaim
// means the unconditional columns cannot be forgotten, and nilling the gated
// ones here — in one place, with a test that holds every pointer field nil —
// means a new gated column cannot be silently echoed either.
func InputFromClaimUngated(c NodeClaim) NodeClaimInput {
	in := InputFromClaim(c)
	in.ChangeoverEvacNodes = nil
	in.ChangeoverEvacDestination = nil
	in.ChangeoverCarryoverDisposition = nil
	in.IndexRobotSupplies = nil
	in.KeyRoute = nil
	in.KeyTask = nil
	in.KeepStaged = nil
	in.ReorderPointSource = nil
	in.AutoReorder = nil
	in.Sequence = nil
	return in
}

// MaterializeClaim is the claim the store would hold after UpsertClaim(in):
// an UPDATE of prior when there is one, an INSERT otherwise. It is the model
// of the store's write, in memory, so a draft flow can be planned before it
// is written — and it is pinned against the real INSERT and UPDATE on the
// plant fixtures (store.TestFlowRoundTrip_StoreAgreesWithTheModel), so the
// model is checked, not trusted.
//
// What it cannot know: a fresh row's id and, when the input is silent about
// it, its sequence (the store assigns the next free slot); both stay zero.
// created_at / updated_at are not written.
func MaterializeClaim(in NodeClaimInput, prior *NodeClaim) NodeClaim {
	var c NodeClaim
	if prior != nil {
		c = *prior
	} else {
		c.ReorderPointSource = "legacy"
		c.ChangeoverCarryoverDisposition = CarryoverReplace
	}
	if in.StyleID != 0 {
		c.StyleID = in.StyleID
	}
	// The eighteen unconditional columns, with the store's own normalisation.
	c.CoreNodeName = in.CoreNodeName
	c.Role = protocol.ClaimRoleConsume
	if in.Role == protocol.ClaimRoleProduce {
		c.Role = protocol.ClaimRoleProduce
	}
	c.SwapMode = in.SwapMode
	c.PayloadCode = in.PayloadCode
	// UOPCapacity is deliberately NOT modelled. The store does not write it:
	// it is resolved from the payload catalog when the row is read, keyed on
	// the payload code above. A pure model has no catalog, so predicting a
	// number here would be predicting the catalog, and the one case it would
	// get wrong is the one that matters — a cell whose payload the composer
	// just changed.
	c.ReorderPoint = in.ReorderPoint
	c.InboundStaging = in.InboundStaging
	c.OutboundStaging = in.OutboundStaging
	c.InboundSource = in.InboundSource
	c.OutboundDestination = in.OutboundDestination
	c.AllowedPayloadCodes = cloneStrings(in.AllowedPayloadCodes)
	c.AutoRequestPayload = in.AutoRequestPayload
	c.EvacuateOnChangeover = in.EvacuateOnChangeover
	c.PairedCoreNode = in.PairedCoreNode
	// A loader's deliveries auto-confirm whatever the claim says: a forklift
	// window has nobody at a screen to confirm one. Asked BY NAME — this is a
	// node-kind question, and the drift guard in protocol/ is right that a raw
	// mode comparison here is a second derivation of it.
	c.AutoConfirm = in.AutoConfirm || in.IsLoaderNode()
	c.LinesideSoftThreshold = in.LinesideSoftThreshold
	c.SecondPairedCoreNode = in.SecondPairedCoreNode
	c.ReuseCompatibleBins = in.ReuseCompatibleBins
	c.AutoPush = in.AutoPush
	// The twelve pointer-gated columns: spoken or left alone.
	if in.ReorderPointSource != nil {
		c.ReorderPointSource = *in.ReorderPointSource
		if c.ReorderPointSource == "" {
			c.ReorderPointSource = "legacy"
		}
	}
	if in.AutoReorder != nil {
		c.AutoReorder = *in.AutoReorder
	}
	if in.KeepStaged != nil {
		c.KeepStaged = *in.KeepStaged
	}
	if in.Sequence != nil && (prior != nil || *in.Sequence > 0) {
		c.Sequence = *in.Sequence
	}
	if in.IndexRobotSupplies != nil {
		c.IndexRobotSupplies = *in.IndexRobotSupplies
	}
	if in.ChangeoverEvacNodes != nil {
		c.ChangeoverEvacNodes = cloneStrings(*in.ChangeoverEvacNodes)
	}
	if in.ChangeoverEvacDestination != nil {
		c.ChangeoverEvacDestination = *in.ChangeoverEvacDestination
	}
	if in.ChangeoverCarryoverDisposition != nil {
		c.ChangeoverCarryoverDisposition = *in.ChangeoverCarryoverDisposition
		if prior == nil && c.ChangeoverCarryoverDisposition == "" {
			c.ChangeoverCarryoverDisposition = CarryoverReplace
		}
	}
	if in.KeyRoute != nil {
		c.KeyRoute = cloneStrings(*in.KeyRoute)
	}
	if in.KeyTask != nil {
		c.KeyTask = *in.KeyTask
	}
	if in.SourcePresetID != nil {
		c.SourcePresetID = clonePtr(in.SourcePresetID)
	}
	if in.SourcePresetVersion != nil {
		c.SourcePresetVersion = clonePtr(in.SourcePresetVersion)
	}
	// Attribution: always written; an empty source reads as admin.
	c.Source = in.Source
	if c.Source == "" {
		c.Source = ClaimSourceAdmin
	}
	c.CalledBy = in.CalledBy
	c.RetiredAt = nil
	return c
}

// cloneStrings copies a list, reading an empty one as nil — the store holds
// an empty list as "" and reads it back as nil, so nil is the canonical empty.
func cloneStrings(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return append([]string(nil), s...)
}

// nonNilStrings is cloneStrings for a value that is SPOKEN: a pointer to an
// empty list still says "clear it", so the list must not be nil.
func nonNilStrings(s []string) []string {
	if len(s) == 0 {
		return []string{}
	}
	return append([]string(nil), s...)
}

func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
