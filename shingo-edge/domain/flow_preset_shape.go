package domain

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"shingo/protocol"
	"shingoedge/domain/flowspec"
)

// flow_preset_shape.go — what a preset's SHAPE is, how two shapes are
// compared, and the key that groups equal ones.
//
// A PRESET IS A SHAPE, NEVER A PART. The shape is a flow's cells minus
// payload_code (and minus Advanced — see StripFlowPayload). Everything that
// follows exists so that one sentence is enforced in exactly one place instead
// of being re-decided by every caller: the create endpoint strips before it
// writes, the drift compare ignores the part, and the candidate grouping keys
// on the stripped form.
//
// DRIFT IS COMPUTED FROM TRUTH (SYNTH R-L1), so the compare takes two live
// flows: the preset's stored cells and the member style's collapsed claims.
// The stored VERSION is never the oracle. `source_preset_id/version` on a
// claim row says where a flow came from; a row that says "I came from v2" is
// not evidence that it still looks like v2, which is the whole reason the
// table earned its place in the round.

// shapeFields are the cell fields a shape is made of.
//
// THE WORDS ARE THE CLAIM'S OWN FIELD NAMES (owner ruling F3, 2026-09-12),
// AND THEY COME FROM ONE TABLE. They were D1's column headings, and those
// headings were invented — a drifted member read `PLN_04 · old bins to` for
// `outbound_destination`. Shingo has had a name for that field since the
// column did, and two names for one field is a second thing to learn and a
// thing to get wrong.
//
// The first version of that fix wrote the right words HERE, in a second table
// beside flowspec's — so "paired position" on this screen was "Paired Core
// Node" in a refusal, and the JS carried a third copy. flowspec.Label is the
// one table now: this names the FIELD and asks for its word, and the model's
// SHAPE_FIELDS reads the same labels out of the generated flowspec block.
//
// payload_code is deliberately absent. So is Advanced: a reorder point is a
// policy on a position, not a shape, and a preset that carried one would apply
// a policy along with a shape without saying so.
var shapeFields = []struct {
	field flowspec.Field
	of    func(FlowCell) string
}{
	{flowspec.SwapMode, func(c FlowCell) string { return string(c.SwapMode) }},
	{flowspec.Role, func(c FlowCell) string { return string(c.Role) }},
	{flowspec.PairedCoreNode, func(c FlowCell) string { return c.PairedCoreNode }},
	{flowspec.SecondPairedCoreNode, func(c FlowCell) string { return c.SecondPairedCoreNode }},
	{flowspec.InboundStaging, func(c FlowCell) string { return c.InboundStaging }},
	{flowspec.OutboundStaging, func(c FlowCell) string { return c.OutboundStaging }},
	{flowspec.InboundSource, func(c FlowCell) string { return c.InboundSource }},
	{flowspec.OutboundDestination, func(c FlowCell) string { return c.OutboundDestination }},
	{flowspec.ChangeoverEvacDestination, func(c FlowCell) string { return c.ChangeoverEvacDestination }},
	{flowspec.ChangeoverEvacNodes, func(c FlowCell) string { return strings.Join(c.ChangeoverEvacNodes, ",") }},
	{flowspec.KeyRoute, func(c FlowCell) string { return strings.Join(c.KeyRoute, " › ") }},
}

// StripFlowPayload is the shape of a flow: a copy with the part removed from
// every cell, and the Advanced policy block dropped.
//
// A COPY. A caller's flow is not this function's to change, and the desktop
// hands it the live draft.
func StripFlowPayload(cells []FlowCell) []FlowCell {
	out := make([]FlowCell, 0, len(cells))
	for _, c := range cells {
		c.PayloadCode = ""
		c.Advanced = nil
		c.ChangeoverEvacNodes = cloneStrings(c.ChangeoverEvacNodes)
		c.KeyRoute = cloneStrings(c.KeyRoute)
		out = append(out, c)
	}
	return out
}

// FlowShapeKey is a stable string that is equal for two flows of the same
// shape and different otherwise. Cells are sorted by position, so the order
// the store happened to return them in is not part of the answer — two
// orderings of one flow would otherwise be two candidate shapes.
func FlowShapeKey(cells []FlowCell) string {
	stripped := StripFlowPayload(cells)
	sort.Slice(stripped, func(i, j int) bool { return stripped[i].CoreNodeName < stripped[j].CoreNodeName })
	var b strings.Builder
	for _, c := range stripped {
		b.WriteString(c.CoreNodeName)
		for _, f := range shapeFields {
			b.WriteByte('\x1f')
			b.WriteString(f.of(c))
		}
		b.WriteByte('\x1e')
	}
	return b.String()
}

// CompareFlowShape reports whether `got` has the preset's shape, and when it
// does not, WHICH fields differ — one entry per difference, in position order,
// worded as the table words them.
//
// The caller shows the first and puts the rest in a title (SPEC D6), so the
// order matters: a member that drifted on one field should say which one, not
// "3 differences".
func CompareFlowShape(preset, got []FlowCell) (same bool, fields []string) {
	byNode := func(cells []FlowCell) map[string]FlowCell {
		m := make(map[string]FlowCell, len(cells))
		for _, c := range StripFlowPayload(cells) {
			m[c.CoreNodeName] = c
		}
		return m
	}
	want, have := byNode(preset), byNode(got)

	// Every position either flow names, once, in one order, so the list reads
	// down the press rather than down whichever map Go walked first.
	names := make([]string, 0, len(want)+len(have))
	seen := map[string]bool{}
	for n := range want {
		if !seen[n] {
			seen[n], names = true, append(names, n)
		}
	}
	for n := range have {
		if !seen[n] {
			seen[n], names = true, append(names, n)
		}
	}
	sort.Strings(names)

	for _, n := range names {
		w, inPreset := want[n]
		g, inFlow := have[n]
		switch {
		case inPreset && !inFlow:
			fields = append(fields, n+" · missing")
		case !inPreset && inFlow:
			fields = append(fields, n+" · not in the preset")
		default:
			for _, f := range shapeFields {
				if f.of(w) != f.of(g) {
					fields = append(fields, n+" · "+flowspec.Label(f.field))
				}
			}
		}
	}
	return len(fields) == 0, fields
}

// PresetShapeJSON is a preset's flow_json: the shape, in the one envelope the
// store validates (`{"cells":[…]}` — ValidateFlowPreset reads that object, and
// a bare array is not it).
func PresetShapeJSON(cells []FlowCell) (string, error) {
	blob, err := json.Marshal(struct {
		Cells []FlowCell `json:"cells"`
	}{Cells: StripFlowPayload(cells)})
	if err != nil {
		return "", fmt.Errorf("marshal preset shape: %w", err)
	}
	return string(blob), nil
}

// ParsePresetShape reads a preset's flow_json back into cells.
//
// IT ACCEPTS ONLY THE ENVELOPE THE STORE VALIDATES, and that is the point: the
// station's strip-card builder used to unmarshal the same string as a BARE
// ARRAY (`service/station_composer.go`, before U10), which never matches what
// CreateFlowPreset validated — so every stored preset failed to parse and was
// silently skipped, and no preset could ever render a card. One parser, one
// envelope, and a test that stores a preset and reads it back.
func ParsePresetShape(flowJSON string) ([]FlowCell, error) {
	var env struct {
		Cells []FlowCell `json:"cells"`
	}
	if err := json.Unmarshal([]byte(flowJSON), &env); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFlowPresetMalformed, err)
	}
	if len(env.Cells) == 0 {
		return nil, ErrFlowPresetMalformed
	}
	return env.Cells, nil
}

// SuggestPresetName is the migration offer's suggested name: THE MODE WORD
// ALONE — `2‑robot index` (owner ruling R5, 2026-09-12).
//
// THE POSITIONS ARE NOT IN THE NAME, because they are not a property of the
// name. Every card and every row draws the shape's positions itself, always
// and beside whatever the preset is called, so a name that carried them said
// them twice on every surface that shows both — and said them WRONG the moment
// an engineer typed a name of their own, which they may. The 200x56 strip card
// could not hold `2‑robot index · PLN_01 / PLN_04` either and ellipsised it to
// `2-robot index · PL...`, which is the mode word with a stump attached.
//
// A SUGGESTION AND NOTHING MORE: the engineer types over it, and nothing reads
// it back. A shape whose cells disagree on a mode has no mode word and so gets
// no suggestion — the naming field opens empty and the engineer says what it
// is, which is better than a name assembled out of the fields that happen to
// be available.
func SuggestPresetName(cells []FlowCell, modeWord func(protocol.SwapMode) string) string {
	var mode protocol.SwapMode
	for _, c := range cells {
		switch {
		case mode == "":
			mode = c.SwapMode
		case c.SwapMode != mode:
			return ""
		}
	}
	if modeWord == nil {
		return ""
	}
	return modeWord(mode)
}

// PresetShapeNodes is the positions a shape names, sorted — what every card
// and row draws beside the name (owner ruling R5). Here rather than in either
// render layer, so the desktop's Shape column, the strip card's second line
// and the apply modal's title are one list and cannot drift apart.
// IN PRESS ORDER, which is the order the operator sees them in front of them.
// pressOrder is the process's position names in sequence; anything not on it
// sorts after, by name, so a shape naming a node the press no longer has is
// still printed rather than dropped.
//
// THERE WERE THREE ORDERS. This sorted lexically, the desktop's Shape column
// sorted by the picture's position index, and the apply modal's title sorted
// lexically again — so D6 could read `PLN_04 / PLN_01` and the modal that
// applied it `PLN_01 / PLN_04`, for the same preset, one click apart. At
// Hopkinsville and Springfield the names happen to sort into press order, so
// nothing on either plant showed it; the day a press is numbered out of order
// is the day the two disagree in front of an engineer.
func PresetShapeNodes(cells []FlowCell, pressOrder []string) []string {
	at := map[string]int{}
	for i, n := range pressOrder {
		at[n] = i
	}
	nodes := make([]string, 0, len(cells))
	for _, c := range cells {
		if c.CoreNodeName != "" {
			nodes = append(nodes, c.CoreNodeName)
		}
	}
	sort.SliceStable(nodes, func(i, j int) bool {
		ai, aok := at[nodes[i]]
		bi, bok := at[nodes[j]]
		if aok != bok {
			return aok // a position the press has comes before one it does not
		}
		if aok && ai != bi {
			return ai < bi
		}
		return nodes[i] < nodes[j]
	})
	return nodes
}

// PresetShapeNodeWord joins them the way every surface prints them.
func PresetShapeNodeWord(cells []FlowCell, pressOrder []string) string {
	return strings.Join(PresetShapeNodes(cells, pressOrder), " / ")
}

// SwapModeWord is the choreography's name AS THE SCREENS SAY IT — the words a
// preset's suggested name is built from, and the ones on the positions table's
// Swaps chip and the strip card.
//
// NOT swapModeLabel (claim_validation.go), which is the VALIDATOR's wording:
// "2-Robot Press Index" is what a refusal says, "2‑robot index" is what a
// control says, and a suggested preset name is a control.
//
// The authority is composer-model.js's MODES, because both surfaces render
// from it; this is the Go copy the server needs to build a name, and
// TestSwapModeWordsMatchTheModel holds the two together. The hyphen in the
// two is U+2011 (non-breaking), exactly as the model spells it — a chip that
// wraps mid-word is the thing it is there to prevent.
func SwapModeWord(mode protocol.SwapMode) string {
	switch mode {
	case protocol.SwapModeTwoRobotPressIndex:
		return "2‑robot index"
	case protocol.SwapModeTwoRobot:
		return "2‑robot swap"
	case protocol.SwapModeSingleRobot:
		return "1‑robot swap"
	case protocol.SwapModeSequential:
		return "Sequential A/B"
	}
	return ""
}
