// flow_preset_service.go — the Presets tab's read, and the two writes behind it.
//
// WHAT A PRESET IS (SYNTH R-L1, locked): a named, versioned, PAYLOAD-FREE
// shape of a flow, owned by one process. Naming is an engineer's act on the
// desktop; the floor never names one.
//
// The three things this file computes, and why each is computed rather than
// stored:
//
//   - MEMBERS — the styles whose live claims point at the preset. Provenance,
//     read off the claim rows.
//   - DRIFTED — the members whose collapsed, payload-stripped shape is no
//     longer the preset's. COMPUTED FROM TRUTH, never from the stored version:
//     a claim that says "I came from v2" is not evidence that it still looks
//     like v2, and treating the version as the oracle is exactly the mistake
//     the round rejected (it reports the correct styles as drifted when a
//     bulk apply redefines the flow under them).
//   - CANDIDATES — the migration offer. One entry per distinct shape among the
//     process's styles that matches no ACTIVE preset, with the styles behind it
//     and a suggested name. Grouping is by shape key, which ignores the part,
//     so a dozen styles running two shapes offer TWO rows and not a dozen.
//
// APPLY IS NOT HERE, and that is deliberate: the desktop applies a preset to
// one style at a time through flow/preview then flow/save, so a preset can
// never change a style without a preview on screen. Nothing in this file
// writes a claim except the provenance stamp.

package service

import (
	"fmt"
	"sort"

	"shingoedge/domain"
	"shingoedge/store/processes"
)

// THE METHODS HANG OFF ProcessService, not a service of their own. A preset is
// per-process configuration and its rows live in the processes aggregate
// (store/processes/flow_presets.go), beside the routing set ProcessService
// already owns — and www's ServiceAccess interface has a width tripwire that
// asks for an owner conversation before it grows. A new accessor for four
// methods on an existing aggregate is exactly the casual widening that guard
// exists to stop.

// PresetsView is the whole GET: the active presets with their members and
// drift, and the shapes nobody has named.
type PresetsView struct {
	Presets    []PresetRow       `json:"presets"`
	Candidates []PresetCandidate `json:"candidates"`
	// StylesWithFlow is how many of the process's live styles have any claim
	// at all — the denominator for "N shapes over M flows", and the number
	// that makes the grouping visible.
	StylesWithFlow int `json:"styles_with_flow"`
}

// PresetRow is one row of the PRESETS section.
type PresetRow struct {
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	Version   int               `json:"version"`
	CreatedBy string            `json:"created_by"`
	CreatedAt string            `json:"created_at"`
	Shape     []domain.FlowCell `json:"shape"`
	// Where is the shape's positions, IN PRESS ORDER, as every surface prints
	// them — the strip card's second line, D6's Shape column and the apply
	// modal's title. Computed here because the order is the process's, and
	// the page has no business re-deriving it: it did, and the Shape column
	// and the modal that applied it sorted differently.
	Where string `json:"where"`
	// Mode is the one swap mode the shape carries, blank when its cells
	// disagree — the same rule the strip card's glyph follows.
	Mode string `json:"mode,omitempty"`
	// Members is every style whose live claims point at this preset, any
	// version. Drifted is the subset whose shape no longer matches, with the
	// differing fields.
	Members []PresetMember `json:"members"`
	Drifted []PresetMember `json:"drifted"`
}

// PresetMember is one style under a preset, and what it looks like now.
type PresetMember struct {
	StyleID int64    `json:"style_id"`
	Name    string   `json:"name"`
	Version int      `json:"version"`
	Fields  []string `json:"fields,omitempty"`
}

// PresetCandidate is one unnamed shape the process already runs.
type PresetCandidate struct {
	ShapeKey      string            `json:"shape_key"`
	SuggestedName string            `json:"suggested_name"`
	Mode          string            `json:"mode,omitempty"`
	Shape         []domain.FlowCell `json:"shape"`
	// Where is the shape's positions in press order, like PresetRow.Where.
	Where       string   `json:"where"`
	Members     []int64  `json:"members"`
	MemberNames []string `json:"member_names"`
}

// styleFlow is one style's collapsed flow, read once and reused by all three
// computations.
type styleFlow struct {
	id    int64
	name  string
	cells []domain.FlowCell
	key   string
	// presetID / presetVersion are the provenance the claims carry, when they
	// agree. They disagree only if a flow was half-applied, which is a state
	// the desktop's one-style-at-a-time apply does not produce.
	presetID      int64
	presetVersion int
}

// PresetsFor is the Presets tab's read.
//
// COST: FOUR QUERIES, AND FLAT IN THE STYLE COUNT. styleFlows below reads the
// styles once and ListLiveClaimsByProcess once, then buckets the claims by
// style in memory; this adds the presets list and the position order. Four at
// 40 styles, four at 400. TestPresetsFor_QueryCount is the pin and prints the
// number on every run.
//
// This said "one claims query per style … ~92 indexed queries" and named a
// measurement in a report to back it. It was never true of this function —
// styleFlows was a per-process read the day it was written — and the pin that
// would have caught it was a bare `n > 6` that printed nothing, so a passing
// run gave nobody a number to check the prose against. It prints one now.
//
// No cache, because a cached drift figure is a stored drift oracle by another
// name, and the round was explicit that drift comes from truth every time it
// is asked.
func (s *ProcessService) PresetsFor(processID int64) (*PresetsView, error) {
	flows, err := s.styleFlows(processID)
	if err != nil {
		return nil, err
	}
	presets, err := s.db.ListFlowPresets(processID, false)
	if err != nil {
		return nil, err
	}
	// The order every shape's positions are printed in. One read, handed to
	// each row: see domain.PresetShapeNodes.
	pressOrder, err := s.pressOrder(processID)
	if err != nil {
		return nil, err
	}

	out := &PresetsView{Presets: []PresetRow{}, Candidates: []PresetCandidate{}}
	for _, f := range flows {
		if len(f.cells) > 0 {
			out.StylesWithFlow++
		}
	}

	// ── the named shapes ─────────────────────────────────────────────────
	activeKeys := map[string]bool{}
	byID := map[int64]processes.FlowPreset{}
	for _, p := range presets {
		byID[p.ID] = p
	}
	for _, p := range presets {
		cells, err := domain.ParsePresetShape(p.FlowJSON)
		if err != nil {
			// A stored preset that does not parse is a row to report, not a
			// row to crash on. It cannot have come from this service.
			continue
		}
		row := PresetRow{
			ID: p.ID, Name: p.Name, Version: p.Version, CreatedBy: p.CreatedBy,
			CreatedAt: p.CreatedAt.Format("01-02"),
			Shape:     cells, Mode: oneMode(cells),
			Where:   domain.PresetShapeNodeWord(cells, pressOrder),
			Members: []PresetMember{}, Drifted: []PresetMember{},
		}
		activeKeys[domain.FlowShapeKey(cells)] = true
		for _, f := range flows {
			if f.presetID != p.ID {
				continue
			}
			m := PresetMember{StyleID: f.id, Name: f.name, Version: f.presetVersion}
			same, fields := domain.CompareFlowShape(cells, f.cells)
			row.Members = append(row.Members, m)
			if !same {
				m.Fields = fields
				row.Drifted = append(row.Drifted, m)
			}
		}
		out.Presets = append(out.Presets, row)
	}

	// ── the shapes nobody has named ──────────────────────────────────────
	//
	// A STYLE ALREADY UNDER AN ACTIVE PRESET IS NOT ON OFFER, even when its
	// shape has drifted away from that preset and so "matches no active
	// preset". §1a's wording allows either reading; the section's own name
	// settles it. This is the MIGRATION offer, and a part carrying provenance
	// has been migrated — what has happened to it since is drift, which the
	// preset's own row reports and whose control is `Apply to parts…`.
	//
	// Offering it twice was worse than noise. The first D6 shot showed the one
	// drifted member of `2‑robot index · PLN_01 / PLN_04` sitting under FOUND
	// IN YOUR FLOWS as a shape to name — with a suggested name of `2‑robot
	// index · PLN_01 / PLN_04`, character for character, because the field it
	// drifted on is not one a suggested name is built from. Naming it would
	// have written version 2 of that preset holding a DIFFERENT shape, under a
	// name that means the first one. Two rows, one name, two shapes, and the
	// engineer with no way to tell from the list which was which.
	seen := map[string]int{} // shape key -> index into out.Candidates
	for _, f := range flows {
		if len(f.cells) == 0 || activeKeys[f.key] {
			continue
		}
		if _, member := byID[f.presetID]; member {
			continue
		}
		i, ok := seen[f.key]
		if !ok {
			shape := domain.StripFlowPayload(f.cells)
			sort.Slice(shape, func(a, b int) bool { return shape[a].CoreNodeName < shape[b].CoreNodeName })
			out.Candidates = append(out.Candidates, PresetCandidate{
				ShapeKey:      f.key,
				SuggestedName: domain.SuggestPresetName(shape, domain.SwapModeWord),
				Mode:          oneMode(shape),
				Shape:         shape,
				Where:         domain.PresetShapeNodeWord(shape, pressOrder),
			})
			i = len(out.Candidates) - 1
			seen[f.key] = i
		}
		out.Candidates[i].Members = append(out.Candidates[i].Members, f.id)
		out.Candidates[i].MemberNames = append(out.Candidates[i].MemberNames, f.name)
	}
	// Biggest offer first: the shape most of the press already runs is the one
	// worth naming.
	sort.SliceStable(out.Candidates, func(a, b int) bool {
		if len(out.Candidates[a].Members) != len(out.Candidates[b].Members) {
			return len(out.Candidates[a].Members) > len(out.Candidates[b].Members)
		}
		return out.Candidates[a].SuggestedName < out.Candidates[b].SuggestedName
	})
	return out, nil
}

// CreateFromStyle names the shape of one style's flow. The part is stripped
// here, so the store's refusal of a payload is a pin on this function rather
// than a path a user can reach.
func (s *ProcessService) CreateFromStyle(processID, styleID int64, name, createdBy string) (int64, error) {
	claims, err := s.db.ListStyleNodeClaims(styleID)
	if err != nil {
		return 0, err
	}
	if len(claims) == 0 {
		return 0, fmt.Errorf("style %d has no flow to name", styleID)
	}
	cells := make([]domain.FlowCell, 0, len(claims))
	for _, c := range claims {
		cells = append(cells, domain.Collapse(c))
	}
	return s.create(processID, name, createdBy, cells, nil)
}

// CreateFromCandidate names one of the migration offer's shapes, and stamps
// the provenance of every style that runs EXACTLY that shape.
func (s *ProcessService) CreateFromCandidate(processID int64, name, shapeKey, createdBy string) (int64, error) {
	flows, err := s.styleFlows(processID)
	if err != nil {
		return 0, err
	}
	var shape []domain.FlowCell
	var members []int64
	for _, f := range flows {
		if f.key != shapeKey || len(f.cells) == 0 {
			continue
		}
		if shape == nil {
			shape = domain.StripFlowPayload(f.cells)
			sort.Slice(shape, func(a, b int) bool { return shape[a].CoreNodeName < shape[b].CoreNodeName })
		}
		members = append(members, f.id)
	}
	if shape == nil {
		return 0, fmt.Errorf("no style of process %d still has that shape — re-read the offer", processID)
	}
	return s.create(processID, name, createdBy, shape, members)
}

// create writes the preset and, when members are given, stamps them.
//
// THE STAMP IS NOT PART OF THE PRESET'S CORRECTNESS. If it fails, the preset
// still exists and the members simply carry no provenance — which the tab
// shows as a preset with no members and the shape still on offer. That is a
// worse screen, not a wrong one, so the error is returned and the preset is
// not rolled back: deleting a named shape because a bookkeeping column did not
// write would lose the engineer's act, which is the only irreplaceable thing
// in this transaction.
func (s *ProcessService) create(processID int64, name, createdBy string, cells []domain.FlowCell, members []int64) (int64, error) {
	flowJSON, err := domain.PresetShapeJSON(cells)
	if err != nil {
		return 0, err
	}
	id, err := s.db.CreateFlowPreset(domain.FlowPresetInput{
		ProcessID: processID, Name: name, FlowJSON: flowJSON, CreatedBy: createdBy,
	})
	if err != nil {
		return 0, err
	}
	p, err := s.db.GetFlowPreset(id)
	if err != nil {
		return id, err
	}
	for _, sid := range members {
		if _, err := s.db.StampClaimPresetProvenance(sid, id, p.Version); err != nil {
			return id, fmt.Errorf("preset %q saved, but style %d kept no provenance: %w", name, sid, err)
		}
	}
	return id, nil
}

// Rename renames every version of a preset's name and nothing else (owner
// ruling R6). Provenance is by id and does not move, so a member keeps
// pointing at the same version of the same shape.
func (s *ProcessService) Rename(id int64, name string) error {
	return s.db.RenameFlowPreset(id, name)
}

// Archive hides a preset version from both surfaces. Members keep their
// provenance — the row stays because a claim points at it.
func (s *ProcessService) Archive(id int64) error { return s.db.ArchiveFlowPreset(id) }

// Get resolves one preset, archived or not, for the apply flow.
func (s *ProcessService) Get(id int64) (*processes.FlowPreset, error) { return s.db.GetFlowPreset(id) }

// pressOrder is the process's position names in the order the press has them.
func (s *ProcessService) pressOrder(processID int64) ([]string, error) {
	nodes, err := s.db.ListProcessNodesByProcess(processID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.CoreNodeName)
	}
	return out, nil
}

// styleFlows collapses every live style of the process once.
//
// TWO QUERIES, NOT N+1. It listed the styles and then read each one's claims
// on its own — the same per-style loop the composer block had, on a store
// pinned to a single connection, for a page that is a list of shapes. One
// process-wide read answers it; the grouping is by style id, in the order
// ListClaims would have given each style.
func (s *ProcessService) styleFlows(processID int64) ([]styleFlow, error) {
	styles, err := s.db.ListStylesByProcess(processID)
	if err != nil {
		return nil, err
	}
	rows, err := processes.ListLiveClaimsByProcess(s.db.DB, processID)
	if err != nil {
		return nil, err
	}
	byStyle := make(map[int64][]processes.NodeClaim, len(styles))
	for _, c := range rows {
		byStyle[c.StyleID] = append(byStyle[c.StyleID], c)
	}
	out := make([]styleFlow, 0, len(styles))
	for _, st := range styles {
		claims := byStyle[st.ID]
		f := styleFlow{id: st.ID, name: st.Name}
		for _, c := range claims {
			f.cells = append(f.cells, domain.Collapse(c))
			// The provenance the claims agree on; a disagreement reads as none.
			id, ver := int64(0), 0
			if c.SourcePresetID != nil {
				id = *c.SourcePresetID
			}
			if c.SourcePresetVersion != nil {
				ver = *c.SourcePresetVersion
			}
			switch {
			case f.presetID == 0 && len(f.cells) == 1:
				f.presetID, f.presetVersion = id, ver
			case f.presetID != id:
				f.presetID, f.presetVersion = 0, 0
			}
		}
		if len(f.cells) > 0 {
			f.key = domain.FlowShapeKey(f.cells)
		}
		out = append(out, f)
	}
	return out, nil
}

// oneMode is the single swap mode a shape carries, or "" when its cells
// disagree — the strip card's rule, reused so the list and the card agree.
func oneMode(cells []domain.FlowCell) string {
	mode := ""
	for _, c := range cells {
		m := string(c.SwapMode)
		switch {
		case mode == "":
			mode = m
		case m != mode:
			return ""
		}
	}
	return mode
}
