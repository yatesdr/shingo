// Package flowspec is the one statement of which claim fields each swap mode
// uses, requires and refuses.
//
// Before it existed the answer was written six times in three languages —
// domain.ValidateNodeClaim, store/processes.UpsertClaim, the changeover
// planner's requiredChangeoverFields, and the admin page's claimFieldVisibility,
// claimForbiddenFields and compare grid — and they disagreed. A claim could pass
// the save-time validator and be refused by the planner minutes later, after
// the operator had pressed START (the 2026-06-23 ALN_001 dropoff-to-nowhere).
//
// Three of those six are gone rather than reconciled: U9d deleted the admin
// page's claim modal, and the composer that replaced it reads this package's
// Document instead of carrying lists of its own. The JS readers named in
// comments below are that one reader seen from four angles — see Document.
//
// TWO TABLES, NOT ONE, and the reason is a code fact: the planner switches on
// the OUTGOING claim's mode and reads fields off both sides of the pair
// (requiredChangeoverFields, changeover_planner.go). Readiness is therefore a
// pairwise question and cannot be asked of a single claim at save time.
//
//   - Steady(role, mode) answers the unary question the save path and the
//     editor ask: for a claim in this mode, what does each field mean.
//   - Changeover(fromMode) answers the pairwise question the planner asks: for
//     a changeover leaving this mode, which side's fields does the builder read.
//
// FAITHFUL, NOT CORRECTED. The tables record what the readers do today,
// divergences included. Where readers disagree about a (mode, field), Steady
// carries the save-time validator's answer and the disagreement is pinned by
// name in the tests — TestFlowspecPinsKnownDisagreements — rather than quietly
// resolved. Resolving one is a behaviour change and gets its own commit.
//
// NO IMPORTS FROM engine, store OR www, and none from domain either: domain
// consults this package, so an import back would be a cycle. The one domain
// constant the planner needs (the press_position marker) is defined here and
// aliased there for the same reason.
package flowspec

import (
	"encoding/json"

	"shingo/protocol"
)

// Need is what a mode makes of a field.
//
// FOUR ANSWERS, NOT THREE. The obvious set — required, used, forbidden — cannot
// say what the editor does today, which is two different things with a hidden
// control: it hides lineside_soft_threshold on a produce claim and CARRIES THE
// STORED VALUE THROUGH, and it hides paired_core_node on a single_robot claim
// and CLEARS IT AT SAVE. One is Unused, the other Forbidden, and collapsing
// them would make the editor's drop note lie in one direction or the other.
type Need uint8

const (
	// Unspecified is the zero value and means the table has no entry. It is
	// never a legitimate answer: the tables are total over Fields() and the
	// tests say so. A consumer that sees it has asked about a field this
	// package does not know.
	Unspecified Need = iota
	// Required: the mode cannot run without it. Refused blank at save; the
	// editor offers it.
	Required
	// Used: the mode reads it. Offered by the editor, saved as entered.
	Used
	// Unused: the mode never reads it. The editor does not offer it and carries
	// the stored value through untouched.
	Unused
	// Forbidden: the mode must not carry it. The editor clears it at save and
	// says so; the server refuses it where it has a rule for the field.
	Forbidden
)

var needNames = map[Need]string{
	Unspecified: "",
	Required:    "required",
	Used:        "used",
	Unused:      "unused",
	Forbidden:   "forbidden",
}

// String satisfies fmt.Stringer. Unspecified renders empty on purpose: it is
// the absence of an answer and must not read like one.
func (n Need) String() string { return needNames[n] }

// MarshalText renders the lower-case name the JSON export and the editor use.
func (n Need) MarshalText() ([]byte, error) { return []byte(n.String()), nil }

// Field names a claim column, by its WIRE name — the json tag on
// NodeClaimInput and the style_node_claims column, which are the same string.
// The editor's camelCase state keys and the planner's diagnostic labels both
// map from this; neither is the identity.
type Field string

// The claim columns the six readers consult. The first five are the ROUTING
// fields — the ones a swap mode's choreography actually drives — and their
// order is load-bearing: both validateSwapModeRouting and
// requiredChangeoverFields report findings in Fields() order, and this
// sequence reproduces every finding list those two produced before the table
// existed (to-Inbound Staging, from-Outbound Staging, from-Outbound Destination
// for single_robot; from-Paired Core Node, from-Outbound Destination,
// to-Inbound Source for sequential). Reorder these and the diagnostics change.
const (
	InboundStaging       Field = "inbound_staging"
	OutboundStaging      Field = "outbound_staging"
	PairedCoreNode       Field = "paired_core_node"
	OutboundDestination  Field = "outbound_destination"
	InboundSource        Field = "inbound_source"
	SecondPairedCoreNode Field = "second_paired_core_node"

	PayloadCode                    Field = "payload_code"
	AllowedPayloadCodes            Field = "allowed_payload_codes"
	UOPCapacity                    Field = "uop_capacity"
	ReorderPoint                   Field = "reorder_point"
	AutoReorder                    Field = "auto_reorder"
	LinesideSoftThreshold          Field = "lineside_soft_threshold"
	Sequence                       Field = "sequence"
	KeepStaged                     Field = "keep_staged"
	EvacuateOnChangeover           Field = "evacuate_on_changeover"
	ChangeoverEvacNodes            Field = "changeover_evac_nodes"
	ChangeoverEvacDestination      Field = "changeover_evac_destination"
	ChangeoverCarryoverDisposition Field = "changeover_carryover_disposition"
	ReuseCompatibleBins            Field = "reuse_compatible_bins"
	IndexRobotSupplies             Field = "index_robot_supplies"
	AutoConfirm                    Field = "auto_confirm"
	AutoRequestPayload             Field = "auto_request_payload"
	AutoPush                       Field = "auto_push"
	KeyRoute                       Field = "key_route"
	KeyTask                        Field = "key_task"
	// ReorderPointSource is HOW the reorder point above was arrived at —
	// "manual", "calculated", "legacy". It is a column FlowAdvanced can write
	// and the only one that had no row here, so its value domain was enforced
	// in the browser and nowhere else.
	ReorderPointSource Field = "reorder_point_source"

	// SwapMode and Role are NOT columns the per-mode matrices consult — they
	// are the two discriminants the matrices are keyed BY, so they are not in
	// fieldOrder and not in Fields(). They are here because they are claim
	// field names that surfaces have to WORD, and the words belong in the one
	// table with the rest (owner ruling F3): the preset compare listed
	// "swaps" and "role" beside "inbound staging" from a second table of its
	// own, so a field's name depended on which screen had printed it.
	SwapMode Field = "swap_mode"
	Role     Field = "role"
)

// fieldLabels is the diagnostic wording for each field — the planner's
// missingField.Name, which the operator sees in a NodeAction error. Mode-
// specific wording ("Back Press Node" for a press-index pair) belongs to the
// reader that speaks it, not to the field.
var fieldLabels = map[Field]string{
	InboundStaging:                 "Inbound Staging",
	OutboundStaging:                "Outbound Staging",
	PairedCoreNode:                 "Paired Core Node",
	OutboundDestination:            "Outbound Destination",
	InboundSource:                  "Inbound Source",
	SecondPairedCoreNode:           "Second Paired Core Node",
	PayloadCode:                    "Payload",
	AllowedPayloadCodes:            "Allowed Payloads",
	UOPCapacity:                    "UOP Capacity",
	ReorderPoint:                   "Reorder Point",
	AutoReorder:                    "Auto Reorder",
	LinesideSoftThreshold:          "Lineside Soft Threshold",
	Sequence:                       "Board Order",
	KeepStaged:                     "Keep Staged",
	EvacuateOnChangeover:           "Evacuate On Changeover",
	ChangeoverEvacNodes:            "Changeover Evac Nodes",
	ChangeoverEvacDestination:      "Changeover Evac Destination",
	ChangeoverCarryoverDisposition: "Carry-over Disposition",
	ReuseCompatibleBins:            "Reuse Compatible Bins",
	IndexRobotSupplies:             "Index Robot Supplies",
	AutoConfirm:                    "Auto Confirm",
	AutoRequestPayload:             "Auto Request Payload",
	AutoPush:                       "Auto Push",
	KeyRoute:                       "Key Route",
	KeyTask:                        "Key Task",
	ReorderPointSource:             "Reorder Point Source",
	SwapMode:                       "Swap Mode",
	Role:                           "Role",
}

// fieldOrder is the canonical order of every field this package knows. See
// the note on the constants for why the first five are fixed.
var fieldOrder = []Field{
	InboundStaging, OutboundStaging, PairedCoreNode, OutboundDestination, InboundSource,
	SecondPairedCoreNode,
	PayloadCode, AllowedPayloadCodes, UOPCapacity, ReorderPoint, AutoReorder,
	LinesideSoftThreshold, Sequence, KeepStaged, EvacuateOnChangeover,
	ChangeoverEvacNodes, ChangeoverEvacDestination, ChangeoverCarryoverDisposition,
	ReuseCompatibleBins, IndexRobotSupplies, AutoConfirm, AutoRequestPayload, AutoPush,
	KeyRoute, KeyTask, ReorderPointSource,
}

// Fields returns every field, in canonical order. A fresh slice each call.
func Fields() []Field {
	out := make([]Field, len(fieldOrder))
	copy(out, fieldOrder)
	return out
}

// RoutingFields returns the five fields a swap mode's choreography drives —
// the ones the save-time routing check and the planner's registry consult —
// in the order both report them.
func RoutingFields() []Field {
	return []Field{InboundStaging, OutboundStaging, PairedCoreNode, OutboundDestination, InboundSource}
}

// Label is the diagnostic name of a field. Unknown fields label as their wire
// name rather than as nothing, so a message about one still names it.
func Label(f Field) string {
	if l, ok := fieldLabels[f]; ok {
		return l
	}
	return string(f)
}

// SwapModePressPosition marks a per-position claim synthesized from a
// press-index parent during a changeover — one physical position treated as an
// independent slot. It is NEVER PERSISTED (UpsertClaim's allowlist rejects it)
// and never reaches Steady; the planner asks Changeover about it. Defined here
// rather than in domain because domain imports this package; domain aliases it.
const SwapModePressPosition protocol.SwapMode = "press_position"

// Roles returns the two claim roles in the order the editor offers them.
func Roles() []protocol.ClaimRole {
	return []protocol.ClaimRole{protocol.ClaimRoleConsume, protocol.ClaimRoleProduce}
}

// ChangeoverModes returns every mode Changeover has a table for: the
// configurable modes, the in-memory press_position marker, and the retired
// "simple", whose row shows what the planner's fallback arm answers for it.
func ChangeoverModes() []protocol.SwapMode {
	return append(protocol.AllSwapModes(), SwapModePressPosition)
}

// Known reports whether mode has a Steady row of its own. The retired "simple"
// does — the editor can still be handed a legacy row in that mode and the
// table says what it does with one — so this is AllSwapModes, not the
// configurable subset.
func Known(mode protocol.SwapMode) bool {
	for _, m := range protocol.AllSwapModes() {
		if m == mode {
			return true
		}
	}
	return false
}

// ── Steady ────────────────────────────────────────────────────────────

// steadyBase is the answer for a field no reader has an opinion about: it is
// offered and saved as entered. Every row below starts from this and overrides.
func steadyBase() map[Field]Need {
	out := make(map[Field]Need, len(fieldOrder))
	for _, f := range fieldOrder {
		out[f] = Used
	}
	// UOPCapacity is Unused in EVERY mode. It is not a claim field any more:
	// the column is dead and the number is resolved from payload_catalog on
	// read, keyed on payload_code, so an editor control over it would take an
	// entry, save without complaint, and change nothing. Unused rather than
	// Forbidden because the stored value is left alone rather than cleared —
	// the old numbers are the only record of how far the copies had drifted.
	out[UOPCapacity] = Unused
	// KeepStaged is Unused in EVERY mode, and this is the rebase carrying a
	// decision main had already made everywhere else.
	//
	// The option is WITHHELD: domain.ValidateNodeClaim, processes.UpsertClaim
	// and the changeover planner all refuse a claim that asks for it, with one
	// message (domain.KeepStagedWithheld), because nothing restages the spare
	// after a changeover. The old claim editor's checkbox was removed in the
	// same change. This table is what the composer offers from, so leaving it
	// Used here would put the control back on the Advanced sheet — an entry
	// whose save the store answers with a refusal.
	//
	// Unused and not Forbidden, for the same reason as UOPCapacity above and
	// the same one main gives: stored rows are left exactly as they are.
	out[KeepStaged] = Unused
	// ReorderPointSource is Unused in EVERY mode. It is a STAMP, not a
	// setting: the calculator writes "calculated" when it sets a point and a
	// hand edit writes "manual", so it says how the number beside it was
	// arrived at. A control over it would let an engineer claim a number was
	// calculated when it was typed.
	//
	// Unused rather than Forbidden, like the two above: stored values are
	// left exactly as they are, and they are the record of where each
	// press's numbers came from.
	out[ReorderPointSource] = Unused
	return out
}

// noChoreography is the profile every mode WITHOUT press positions, an A/B
// pair or a staging hop shares: the fields those choreographies drive are
// cleared at save and refused where the server has a rule (marked positions
// and the index-robot flip apply to a press-index cell only; a carry-over
// disposition applies to marked positions only, so it is refused non-default
// wherever marks are). Modes that use some of these override afterwards.
func noChoreography(m map[Field]Need) {
	m[PairedCoreNode] = Forbidden
	m[SecondPairedCoreNode] = Forbidden
	m[ReuseCompatibleBins] = Forbidden
	m[IndexRobotSupplies] = Forbidden
	m[ChangeoverEvacNodes] = Forbidden
	m[ChangeoverCarryoverDisposition] = Forbidden
	m[AutoPush] = Forbidden
}

// steadyRows is Steady's table: one row per mode, role-neutral. The two
// role-dependent entries are applied on top in Steady itself.
//
// EVERY ENTRY IS WHAT A READER DOES TODAY. Required entries come from
// validateSwapModeRouting and ValidateNodeClaim's payload rule; Forbidden
// entries from ValidateNodeClaim's three refusals and the mode-change wipe
// (composer-model.js's clearForbidden and clearForbiddenAdvanced); Unused
// entries from the fields the composer draws no control for (rowFields for the
// positions table, advancedShows for the sheet) and does not wipe. The
// divergences are marked D1..D6 and pinned in the tests; nothing here resolves
// one.
//
// THOSE FOUR JS READERS ALL DERIVE FROM THIS TABLE, which is why the list above
// reads as one rule rather than four. It used to name claimForbiddenFields and
// claimFieldVisibility instead — hand-written lists on the admin page's claim
// modal, a second copy of this table that could disagree with it. U9d deleted
// that page (processes.js, docs/ui-style-guide.md's worked example) and the
// composer asks Steady directly, so the shape of the D-list changed with it:
// a divergence can now only be between this table and a GO reader.
func steadyRows() map[protocol.SwapMode]map[Field]Need {
	rows := map[protocol.SwapMode]map[Field]Need{}

	// single_robot: one robot does the whole swap and needs somewhere to park
	// the incoming bin, somewhere to put the outgoing one mid-swap, and
	// somewhere to take it home to.
	//
	// D1 RESOLVED: OutboundDestination is Required at save, as the planner has
	// always required it on the outgoing claim (Changeover). Before this a
	// claim with no destination saved clean and was refused after the operator
	// pressed START.
	sr := steadyBase()
	noChoreography(sr)
	sr[PayloadCode] = Required
	sr[InboundStaging] = Required
	sr[OutboundStaging] = Required
	sr[OutboundDestination] = Required
	rows[protocol.SwapModeSingleRobot] = sr

	// two_robot: robot A waits at inbound staging until robot B clears the
	// line; robot B takes the old bin straight out to the outbound
	// destination, so outbound staging is ignored by the builder and the
	// editor clears it.
	//
	// D2 RESOLVED: OutboundDestination is Required at save, as D1. The
	// dispatcher already refused to build robot B's leg without one; the
	// refusal now arrives on the field, before START.
	tr := steadyBase()
	noChoreography(tr)
	tr[PayloadCode] = Required
	tr[InboundStaging] = Required
	tr[OutboundStaging] = Forbidden
	tr[OutboundDestination] = Required
	rows[protocol.SwapModeTwoRobot] = tr

	// two_robot_press_index: the cell with positions. Everything the
	// noChoreography profile forbids is this mode's to use.
	//
	// D5 RESOLVED BY DELETION, and it is the only one on the list that no code
	// change closed. InboundStaging / OutboundStaging are Used here — the
	// staged tooling changeover reads to-claim InboundStaging
	// (planKeepStagedAction) and the composer draws both columns. The
	// disagreement was the claim modal's claimForbiddenFields, a hand-written
	// drop list that cleared both at save whatever this table said. U9d deleted
	// that modal; what replaced it wipes a field only where this table says
	// Forbidden (composer-model.js clearForbidden), so a Used column is now
	// kept by construction. Pinned in
	// www/static/js/pages/composer-fields.characterization.test.js, which
	// derives its expectation from Steady rather than listing fields.
	pi := steadyBase()
	pi[PayloadCode] = Required
	pi[PairedCoreNode] = Required
	pi[OutboundDestination] = Required
	pi[AutoPush] = Forbidden
	// ReuseCompatibleBins is Unused, and press-index was its only user. The
	// no-swap shortcut it armed is baked in (2026-09-09): a drained produce
	// position whose next style makes the same part is not swapped, without
	// anyone opting in. Unused rather than Forbidden on purpose — the seven
	// Hopkinsville rows that carry it keep their value, because clearing a
	// column at save is a data change and this is a UI one.
	pi[ReuseCompatibleBins] = Unused
	rows[protocol.SwapModeTwoRobotPressIndex] = pi

	// sequential: A/B cycling by direct trips. A partner position, where the
	// old bin goes and where the new carrier comes from; no staging hop.
	sq := steadyBase()
	noChoreography(sq)
	sq[PayloadCode] = Required
	sq[PairedCoreNode] = Required
	sq[OutboundDestination] = Required
	sq[InboundSource] = Required
	sq[InboundStaging] = Forbidden
	sq[OutboundStaging] = Forbidden
	sq[KeepStaged] = Unused // withheld in every mode; see steadyBase
	rows[protocol.SwapModeSequential] = sq

	// manual_swap: a loader or unloader card. Core owns the loader's payload
	// set from the loader board, so the edge-side payload is cleared at save
	// and the groups the board owns are hidden with their values carried
	// through. A loader does not drive, so key routes are refused.
	//
	// D6: OutboundDestination is Required (the store and the validator both
	// refuse a blank one — the post-swap bin has nowhere to go) and the editor
	// HIDES the group; the stored value round-trips through the hidden input.
	ms := steadyBase()
	noChoreography(ms)
	ms[OutboundDestination] = Required
	ms[PayloadCode] = Forbidden
	ms[KeyRoute] = Forbidden
	ms[KeyTask] = Forbidden
	ms[AutoPush] = Used // consume only; produce is overridden in Steady
	for _, f := range []Field{
		UOPCapacity, ReorderPoint, AutoReorder, LinesideSoftThreshold,
		InboundStaging, OutboundStaging, KeepStaged, InboundSource,
		EvacuateOnChangeover, ChangeoverEvacDestination,
		AutoConfirm, AutoRequestPayload,
	} {
		ms[f] = Unused
	}
	rows[protocol.SwapModeManualSwap] = ms

	// simple: RETIRED as a configurable mode; the save path refuses it before
	// asking about routing. The row exists because the editor can still be
	// handed a legacy row in this mode, and this is what it does with one: no
	// choreography fields, no staging, and the staging values cleared at save
	// exactly as for sequential. It is also the answer for a mode this package
	// does not know at all — see Steady.
	simple := steadyBase()
	noChoreography(simple)
	simple[PayloadCode] = Required
	simple[InboundStaging] = Forbidden
	simple[OutboundStaging] = Forbidden
	simple[KeepStaged] = Unused // withheld in every mode; see steadyBase
	rows[protocol.SwapModeSimple] = simple

	return rows
}

// Steady answers the unary question: for a claim of this role in this mode,
// what does each field mean. Total over Fields(); a fresh map each call.
//
// ROLE. Only two entries depend on it: the lineside soft threshold is a
// consume-side idea and is not offered on a produce claim, and auto-push is a
// consume manual_swap (unloader) fact. A blank or unknown role reads as
// consume, which is what UpsertClaim stores for one.
//
// UNKNOWN MODE. A mode with no row — blank, a typo, the in-memory
// press_position marker — answers as the retired "simple" row does: no
// choreography-specific field is used. That is exactly what every reader did
// before the table existed (a switch with no matching arm, a set of mode
// booleans all false), so the findings that accompany the swap_mode refusal
// keep their shape. Use Known to tell the cases apart.
// steadyTable is steadyRows built ONCE. It is a constant function of the
// package's own tables — nothing outside can change what it answers — and it
// was being rebuilt on every call: eleven maps of twenty-six entries each,
// per validated claim, per cell, per preview. The composer previews every
// 400 ms while a flow is edited.
//
// Steady still copies the row it hands out, so a caller that writes into its
// answer cannot reach this.
var steadyTable = steadyRows()

func Steady(role protocol.ClaimRole, mode protocol.SwapMode) map[Field]Need {
	rows := steadyTable
	row, ok := rows[mode]
	if !ok {
		row = rows[protocol.SwapModeSimple]
	}
	out := make(map[Field]Need, len(row))
	for f, n := range row {
		out[f] = n
	}
	if role != protocol.ClaimRoleProduce {
		role = protocol.ClaimRoleConsume
	}
	if role == protocol.ClaimRoleProduce {
		out[LinesideSoftThreshold] = Unused
		if mode == protocol.SwapModeManualSwap {
			out[AutoPush] = Forbidden
		}
	}
	return out
}

// ── Changeover ────────────────────────────────────────────────────────

// Side names which claim of a changeover pair a field is read from.
type Side string

const (
	SideFrom Side = "from" // the outgoing style's claim
	SideTo   Side = "to"   // the incoming style's claim
)

// SideField is one (side, field) the planner's builder reads.
type SideField struct {
	Side  Side
	Field Field
}

// ChangeoverOrder returns every (side, field) pair in the order a reader
// should report them: Fields() order, from-side before to-side. Since each
// routing field is read from exactly one side in every mode, this reproduces
// the planner's historical diagnostic order (see the note on the constants).
func ChangeoverOrder() []SideField {
	out := make([]SideField, 0, 2*len(fieldOrder))
	for _, f := range fieldOrder {
		out = append(out, SideField{SideFrom, f}, SideField{SideTo, f})
	}
	return out
}

// Changeover answers the pairwise question: for a changeover LEAVING fromMode,
// which side's fields does the per-mode builder need. Mirrors
// requiredChangeoverFields' registry, whose comments are the source of each
// entry; a missing Required entry is what the planner turns into a NodeAction
// error after the operator has pressed START.
//
// The map lists only the fields the builder reads; absent means the builder
// does not look. A fresh, non-nil map each call — manual_swap returns an empty
// one because a loader does not go through changeover, and an unknown mode
// (the retired "simple" included) returns single_robot's table, which is the
// planner's fallback arm.
func Changeover(fromMode protocol.SwapMode) map[SideField]Need {
	switch fromMode {
	case protocol.SwapModeSingleRobot:
		// Stage + line-side swap: the incoming bin is staged at the to-claim's
		// inbound staging, the old bin parks at the from-claim's outbound
		// staging mid-swap and ends at its outbound destination.
		return map[SideField]Need{
			{SideTo, InboundStaging}:        Required,
			{SideFrom, OutboundStaging}:     Required,
			{SideFrom, OutboundDestination}: Required,
		}
	case protocol.SwapModeTwoRobot:
		// Pre-stage + ready wait; order B goes straight to the destination.
		return map[SideField]Need{
			{SideTo, InboundStaging}:        Required,
			{SideFrom, OutboundDestination}: Required,
		}
	case protocol.SwapModeTwoRobotPressIndex:
		// Same-bin-type press index. The different-bin-type case fans out to
		// press_position claims before the registry is consulted. The third
		// position is optional (2- vs 3-position layout), and the staged
		// tooling changeover stages the incoming style at the to-claim's
		// inbound staging when one is set.
		return map[SideField]Need{
			{SideFrom, PairedCoreNode}:       Required,
			{SideFrom, OutboundDestination}:  Required,
			{SideFrom, SecondPairedCoreNode}: Used,
			{SideTo, InboundStaging}:         Used,
		}
	case SwapModePressPosition:
		// One synthesized position: a full swap needs where the old bin goes
		// and where the new one comes from; the half cases delegate to the
		// Drop/Add builders, which check their own fields.
		return map[SideField]Need{
			{SideFrom, OutboundDestination}: Required,
			{SideTo, InboundSource}:         Required,
		}
	case protocol.SwapModeSequential:
		// Direct trips, no staging hop: the A/B partner, where evacuated bins
		// go, and where new bins come from.
		return map[SideField]Need{
			{SideFrom, PairedCoreNode}:      Required,
			{SideFrom, OutboundDestination}: Required,
			{SideTo, InboundSource}:         Required,
		}
	case protocol.SwapModeManualSwap:
		// A loader does not go through changeover.
		return map[SideField]Need{}
	default:
		// "simple" or unrecognised: the planner shares single_robot's fields.
		return Changeover(protocol.SwapModeSingleRobot)
	}
}

// ── Export ────────────────────────────────────────────────────────────

// Document is the JSON shape of both tables, for the readers that are not Go.
//
// ONE READER, AND IT IS THE COMPOSER'S MODEL. composer-model.js takes this as
// its oracle: steadyRow looks a (role, mode) row up in it, and rowFields,
// advancedShows, clearForbidden and clearForbiddenAdvanced are all that row
// read four ways. This said the admin page derived claimFieldVisibility and
// claimForbiddenFields from it and that the composer's model "will" — both
// halves are now wrong in the same direction: U9d deleted the admin page's
// claim modal and the model is the reader that arrived.
//
// Keys are wire names; needs are their lower-case names.
type Document struct {
	Fields        []Field                                                     `json:"fields"`
	RoutingFields []Field                                                     `json:"routing_fields"`
	Labels        map[Field]string                                            `json:"labels"`
	Roles         []protocol.ClaimRole                                        `json:"roles"`
	Modes         []protocol.SwapMode                                         `json:"modes"`
	Steady        map[protocol.ClaimRole]map[protocol.SwapMode]map[Field]Need `json:"steady"`
	Changeover    map[protocol.SwapMode]map[Side]map[Field]Need               `json:"changeover"`
}

// Export renders both tables. Steady is rendered for every role and every mode
// with a row of its own (Known); Changeover for ChangeoverModes(). Deterministic:
// slices are in canonical order and encoding/json sorts map keys.
func Export() Document {
	doc := Document{
		Fields:        Fields(),
		RoutingFields: RoutingFields(),
		Labels:        map[Field]string{},
		Roles:         Roles(),
		Modes:         protocol.ConfigurableSwapModes(),
		Steady:        map[protocol.ClaimRole]map[protocol.SwapMode]map[Field]Need{},
		Changeover:    map[protocol.SwapMode]map[Side]map[Field]Need{},
	}
	for f, l := range fieldLabels {
		doc.Labels[f] = l
	}
	for _, role := range Roles() {
		doc.Steady[role] = map[protocol.SwapMode]map[Field]Need{}
		for _, mode := range protocol.AllSwapModes() {
			doc.Steady[role][mode] = Steady(role, mode)
		}
	}
	for _, mode := range ChangeoverModes() {
		sides := map[Side]map[Field]Need{SideFrom: {}, SideTo: {}}
		for sf, n := range Changeover(mode) {
			sides[sf.Side][sf.Field] = n
		}
		doc.Changeover[mode] = sides
	}
	return doc
}

// ExportJSON is Export as indented JSON with a trailing newline — the exact
// bytes of the golden flowspec.json and of the block embedded in the admin
// page, so a drift test can compare them verbatim.
func ExportJSON() ([]byte, error) {
	b, err := json.MarshalIndent(Export(), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// RequiredFields lists the Required entries of a Steady row in canonical
// order — the shape the agreement tests compare against a validator's
// findings.
func RequiredFields(row map[Field]Need) []Field {
	var out []Field
	for _, f := range fieldOrder {
		if row[f] == Required {
			out = append(out, f)
		}
	}
	return out
}

// RequiredSideFields lists the Required entries of a Changeover row in
// ChangeoverOrder — the shape requiredChangeoverFields reports.
func RequiredSideFields(row map[SideField]Need) []SideField {
	var out []SideField
	for _, sf := range ChangeoverOrder() {
		if row[sf] == Required {
			out = append(out, sf)
		}
	}
	return out
}
