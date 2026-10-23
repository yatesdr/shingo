package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// flow_mint.go — a style from (recipe, parts): the floor's one write, and the
// desktop's "New part flow" (pick-a-part brief, unit U3).
//
// THE FLOOR DESCRIBES WHAT IT WANTS; SHINGO FINDS THE ENGINEER'S RECIPE. The
// station sends a recipe id and the parts the operator tapped, in tap order —
// never cells. Everything a flow needs beyond that is decided here, once:
//
//   - RESOLVE: a live style of the process whose claims carry exactly these
//     parts is the answer, and nothing is written (Minted=false). Parts first,
//     provenance never: a plant style born before presets has none, and
//     resolving by provenance would mint a duplicate of every one of them.
//   - PLACE: each part goes to the node it last ran on here if that node is a
//     slot of the recipe; the rest go to the free slots in picture order, in
//     tap order (owner ruling R-M). The desktop may name a node per part.
//   - EXPAND: the recipe's cells with the parts on their slots, the recipe's
//     role kept, and the minted policy: no auto-reorder, reorder point 0,
//     source 'unset' (R-H / R-L) — the operator reorders by hand until an
//     engineer sets it.
//   - CREATE: the style row and its claims in ONE transaction — SaveFlow's own
//     body (saveFlowTx) — named per R-D, provenance stamped.
//
// MINT AT GO, NEVER AT A TAP. flow/preview takes the same request and draws
// the plan before anything exists; backing out leaves nothing behind.

// PartPick is one part the floor or the desktop picked. Node is the desktop's
// to set; the station sends codes only and the server places them.
type PartPick struct {
	Code string `json:"code"`
	Node string `json:"node,omitempty"`
}

// MintRequest is POST /processes/{id}/flow/mint. Source and CalledBy are the
// handler's, from the authenticated caller, never the body's (owner ruling R1).
type MintRequest struct {
	PresetID  int64      `json:"preset_id"`
	Parts     []PartPick `json:"parts"`
	StationID int64      `json:"station_id"`
	// StyleID names a style outright: the operator tapped it in the
	// list, so nothing is resolved, placed or minted — the answer is that
	// style. Parts and PresetID are ignored when it is set.
	StyleID int64 `json:"style_id,omitempty"`
	// Split is One Part, Two Spots: the one part picked goes on both produce
	// slots of a two-slot recipe, each stroke's two parts to two bins.
	Split bool `json:"split,omitempty"`
	// New is "Set up new": the operator saw the styles these parts
	// already run in and chose to set up another, so the parts are not
	// resolved to one of them. The name takes the recipe's on a collision.
	New bool `json:"new,omitempty"`
	// Overrides are the Spots step's moves (R-K): a node the recipe names →
	// the node it should use instead. Same choreography, different core node,
	// and only where the engineer's recipes allow it (domain.RecipeSpots,
	// checked by applyMoves). A station on a cell whose HMI setup is off
	// cannot mint at all (gateRefusal), moved or not.
	Overrides map[string]string `json:"overrides,omitempty"`
	Source    string            `json:"-"`
	CalledBy  string            `json:"-"`
}

// MintResult is what the mint did. Placement maps each node that took a part
// to the part, for the Go sentence ("PANEL-A on PLN_001").
type MintResult struct {
	StyleID     int64             `json:"style_id"`
	StyleName   string            `json:"style_name"`
	Fingerprint string            `json:"fingerprint"`
	Minted      bool              `json:"minted"`
	Placement   map[string]string `json:"placement"`
	// Running says the parts resolved to the style on the press: there is no
	// changeover to start, and the Go screen says so instead of offering one.
	Running bool `json:"running,omitempty"`
}

// MintRefusal is a mint the request cannot make: the recipe does not fit the
// parts or names a node the process no longer has, a part names a node no
// slot has, a move goes somewhere the recipes do not allow. 422, by name.
type MintRefusal struct{ Reason string }

func (e *MintRefusal) Error() string { return e.Reason }

// ErrMintRecipe: the recipe id is not a live recipe of this process.
var ErrMintRecipe = errors.New("no such recipe on this process")

// mintPlan is a mint decided and not yet written. nodes and routing are the
// process's node list and routing rows, as the recipe was read against them.
type mintPlan struct {
	process   *processes.Process
	preset    processes.FlowPreset
	existing  *processes.Style
	cells     []domain.FlowCell
	placement map[string]string
	name      string
	moved     bool
	nodes     []processes.Node
	routing   []domain.RoutingNode
}

// MintedReorderSource is the reorder_point_source a minted style's claims
// carry: no reorder point has been set, by anybody (R-L). The replenishment
// page and the Advanced pill read it.
const MintedReorderSource = "unset"

// planMint resolves, places and expands. It writes nothing.
func (e *Engine) planMint(processID int64, req MintRequest) (*mintPlan, error) {
	if req.StyleID != 0 {
		return e.planSaved(processID, req.StyleID)
	}
	process, err := e.db.GetProcess(processID)
	if err != nil {
		return nil, err
	}
	codes, err := pickedCodes(req.Parts)
	if err != nil {
		return nil, err
	}
	plan := &mintPlan{process: process, moved: len(req.Overrides) > 0}
	styles, err := e.db.ListStylesByProcess(processID)
	if err != nil {
		return nil, err
	}
	live, err := processes.ListLiveClaimsByProcess(e.db.DB, processID)
	if err != nil {
		return nil, err
	}
	if req.Split && len(codes) != 1 {
		return nil, &MintRefusal{Reason: "One Part, Two Spots takes one part"}
	}
	spots := len(codes)
	if req.Split {
		// The one part fills both slots: placed and expanded as the same code
		// twice.
		spots = 2
		codes = []string{codes[0], codes[0]}
		req.Parts = []PartPick{{Code: codes[0]}, {Code: codes[0]}}
	}
	if !plan.moved && !req.New {
		if st := e.resolveByParts(process, styles, live, codes, spots); st != nil {
			plan.resolved(st, live)
			return plan, nil
		}
	}
	cells, slots, err := e.recipeSlots(processID, req.PresetID, spots, plan)
	if err != nil {
		return nil, err
	}
	if req.Split && countRole(slots, protocol.ClaimRoleProduce) != 2 {
		return nil, &MintRefusal{Reason: "recipe " + plan.preset.Name + " has no two spots the press makes parts into"}
	}
	slotOf, err := placeParts(req.Parts, codes, slots, live)
	if err != nil {
		return nil, err
	}
	plan.cells, plan.placement = expandRecipe(cells, slots, codes, slotOf)
	if plan.moved {
		if err := e.moveSpots(plan, req.Overrides, live); err != nil {
			return nil, err
		}
	}
	roles := make([]protocol.ClaimRole, len(codes))
	ordered := make([]string, len(codes))
	for i := range codes {
		// Slot order, not tap order: the name reads the cell the way the
		// picture does, so two taps of one pair name one style.
		ordered[slotOf[i]] = codes[i]
		roles[slotOf[i]] = slots[slotOf[i]].Role
	}
	base := domain.MintedStyleName(ordered, roles)
	if req.Split {
		base = codes[0] + " (two spots)"
	}
	plan.name = uniqueStyleName(base, plan.preset.Name, styles)
	return plan, nil
}

// resolved makes the plan the existing style st: nothing to write, the
// placement read off its live claims.
func (plan *mintPlan) resolved(st *processes.Style, live []processes.NodeClaim) {
	plan.existing, plan.name = st, st.Name
	plan.placement = map[string]string{}
	for _, c := range live {
		if c.StyleID == st.ID && c.PayloadCode != "" {
			plan.placement[c.CoreNodeName] = c.PayloadCode
		}
	}
}

// planSaved is the plan for a style named by id: that style, live and
// this process's, as it is.
func (e *Engine) planSaved(processID, styleID int64) (*mintPlan, error) {
	process, err := e.db.GetProcess(processID)
	if err != nil {
		return nil, err
	}
	styles, err := e.db.ListStylesByProcess(processID)
	if err != nil {
		return nil, err
	}
	for i := range styles {
		if styles[i].ID != styleID {
			continue
		}
		live, err := processes.ListLiveClaimsByProcess(e.db.DB, processID)
		if err != nil {
			return nil, err
		}
		plan := &mintPlan{process: process}
		plan.resolved(&styles[i], live)
		return plan, nil
	}
	return nil, &MintRefusal{Reason: "that style is not on this process"}
}

func countRole(slots []domain.Slot, role protocol.ClaimRole) int {
	n := 0
	for _, s := range slots {
		if s.Role == role {
			n++
		}
	}
	return n
}

// pickedCodes is the request's part codes, trimmed, in tap order: each named,
// none twice, at least one.
func pickedCodes(picks []PartPick) ([]string, error) {
	codes := make([]string, 0, len(picks))
	seen := map[string]bool{}
	for _, p := range picks {
		code := strings.TrimSpace(p.Code)
		if code == "" {
			return nil, &MintRefusal{Reason: "a picked part has no code"}
		}
		if seen[code] {
			return nil, &MintRefusal{Reason: code + " was picked twice"}
		}
		seen[code] = true
		codes = append(codes, code)
	}
	if len(codes) == 0 {
		return nil, &MintRefusal{Reason: "pick at least one part"}
	}
	return codes, nil
}

// recipeSlots reads the recipe (onto plan.preset) and its slots in picture
// order. It refuses a recipe naming a node the process no longer has, by name,
// before anything is placed (owner, 2026-10-22: refuse and flag, never carry
// the last style's node), and a recipe whose slot count is not the number of
// parts. The node list and routing rows it reads go on the plan; the routing
// read is the one statement the stale check costs a mint.
func (e *Engine) recipeSlots(processID, presetID int64, parts int, plan *mintPlan) ([]domain.FlowCell, []domain.Slot, error) {
	preset, err := e.db.GetFlowPreset(presetID)
	if err != nil || preset == nil || preset.ProcessID != processID || preset.ArchivedAt != nil {
		return nil, nil, ErrMintRecipe
	}
	plan.preset = *preset
	cells, err := domain.ParsePresetShape(preset.FlowJSON)
	if err != nil {
		return nil, nil, err
	}
	nodes, err := e.db.ListProcessNodesByProcess(processID)
	if err != nil {
		return nil, nil, err
	}
	routing, err := e.db.ListRoutingNodes(processID)
	if err != nil {
		return nil, nil, err
	}
	plan.nodes, plan.routing = nodes, routing
	if stale := domain.StaleNodes(cells, domain.RoutingSetOf(nodes, routing)); len(stale) > 0 {
		return nil, nil, &MintRefusal{Reason: domain.StaleSentence(preset.Name, stale[0])}
	}
	order := map[string]int{}
	for i, n := range nodes {
		order[n.CoreNodeName] = i
	}
	slots := domain.PayloadSlots(cells)
	sort.SliceStable(slots, func(i, j int) bool {
		return pictureIndex(order, slots[i].Nodes[0]) < pictureIndex(order, slots[j].Nodes[0])
	})
	if len(slots) != parts {
		return nil, nil, &MintRefusal{Reason: fmt.Sprintf("recipe %s takes %d part%s; %d %s picked",
			preset.Name, len(slots), plural(len(slots)), parts, wasWere(parts))}
	}
	return cells, slots, nil
}

// expandRecipe puts each part on its slot's positions and gives every cell the
// minted policy (R-H / R-L): no auto-reorder, reorder point 0, source 'unset'.
// It returns the cells and the placement (each slot's first position → part).
func expandRecipe(cells []domain.FlowCell, slots []domain.Slot, codes []string, slotOf []int) ([]domain.FlowCell, map[string]string) {
	placement := map[string]string{}
	partAt := map[string]string{}
	for i, code := range codes {
		for _, n := range slots[slotOf[i]].Nodes {
			partAt[n] = code
		}
		placement[slots[slotOf[i]].Nodes[0]] = code
	}
	out := make([]domain.FlowCell, 0, len(cells))
	for _, c := range cells {
		c.PayloadCode = partAt[c.CoreNodeName]
		c.Advanced = &domain.FlowAdvanced{ReorderPointSource: MintedReorderSource, CarryoverDisposition: domain.CarryoverReplace}
		out = append(out, c)
	}
	return out, placement
}

// resolveByParts is the live style whose claims carry exactly these parts on
// this many spots (payload slots). The spot count is what tells One Part, One
// Spot from One Part, Two Spots: both run the one part, on one slot or two.
// Several can match (a retool runs the same part on the same press): the one
// on the press first, then the one changed over to most recently, then the
// oldest.
func (e *Engine) resolveByParts(process *processes.Process, styles []processes.Style, live []processes.NodeClaim, codes []string, spots int) *processes.Style {
	want := map[string]bool{}
	for _, c := range codes {
		want[c] = true
	}
	parts := map[int64]map[string]bool{}
	cells := map[int64][]domain.FlowCell{}
	for _, c := range live {
		cells[c.StyleID] = append(cells[c.StyleID], domain.Collapse(c))
		if c.PayloadCode == "" {
			continue
		}
		if parts[c.StyleID] == nil {
			parts[c.StyleID] = map[string]bool{}
		}
		parts[c.StyleID][c.PayloadCode] = true
	}
	var fits []processes.Style
	for _, st := range styles {
		have := parts[st.ID]
		if len(have) != len(want) || len(domain.PayloadSlots(cells[st.ID])) != spots {
			continue
		}
		same := true
		for c := range want {
			if !have[c] {
				same = false
				break
			}
		}
		if same {
			fits = append(fits, st)
		}
	}
	if len(fits) == 0 {
		return nil
	}
	if process.ActiveStyleID != nil {
		for i := range fits {
			if fits[i].ID == *process.ActiveStyleID {
				return &fits[i]
			}
		}
	}
	if recent, err := processes.ListRecentChangeovers(e.db.DB, process.ID, 50); err == nil {
		for _, co := range recent {
			for i := range fits {
				if co.ToStyleID == fits[i].ID {
					return &fits[i]
				}
			}
		}
	}
	sort.Slice(fits, func(i, j int) bool { return fits[i].ID < fits[j].ID })
	return &fits[0]
}

// placeParts returns, per picked part (tap order), the index of the slot it
// takes: the node the desktop named; else the node it last ran on here, if
// that node is a free slot's; else the first free slot in picture order.
func placeParts(picks []PartPick, codes []string, slots []domain.Slot, live []processes.NodeClaim) ([]int, error) {
	out := make([]int, len(codes))
	taken := make([]bool, len(slots))
	slotWith := func(node string) int {
		for i, s := range slots {
			for _, n := range s.Nodes {
				if n == node {
					return i
				}
			}
		}
		return -1
	}
	pending := []int{}
	for i, p := range picks {
		if p.Node == "" {
			pending = append(pending, i)
			continue
		}
		s := slotWith(strings.TrimSpace(p.Node))
		if s < 0 || taken[s] {
			return nil, &MintRefusal{Reason: fmt.Sprintf("%s cannot go on %s: it is not a free slot of this recipe", codes[i], p.Node)}
		}
		out[i], taken[s] = s, true
	}
	var rest []int
	for _, i := range pending {
		placed := false
		for _, c := range live {
			if c.PayloadCode != codes[i] {
				continue
			}
			if s := slotWith(c.CoreNodeName); s >= 0 && !taken[s] {
				out[i], taken[s], placed = s, true, true
				break
			}
		}
		if !placed {
			rest = append(rest, i)
		}
	}
	for _, i := range rest {
		for s := range slots {
			if !taken[s] {
				out[i], taken[s] = s, true
				break
			}
		}
	}
	return out, nil
}

// moveSpots applies the Spots step's moves to the plan, after checking them
// against the recipe's places. The rule reads the process's enabled recipes:
// one statement, on a moved mint or preview only. The node list, the routing
// rows and the claims are the plan's own.
func (e *Engine) moveSpots(plan *mintPlan, moves map[string]string, live []processes.NodeClaim) error {
	rows, err := e.db.ListFlowPresets(plan.process.ID, false)
	if err != nil {
		return err
	}
	enabled, versions := domain.EnabledRecipes(rows)
	rules := domain.SpotRules{Positions: plan.nodes, Lanes: domain.StagingLanes(plan.routing)}
	for _, p := range enabled {
		// A recipe that does not parse grants nothing; the composer read
		// logs it and offers no card for it.
		if cells, err := domain.ParsePresetShape(p.FlowJSON); err == nil {
			rules.Enabled = append(rules.Enabled, cells)
		}
	}
	byStyle := map[int64][]processes.NodeClaim{}
	for _, c := range live {
		byStyle[c.StyleID] = append(byStyle[c.StyleID], c)
	}
	used := domain.RecipeUsedClaims(versions[plan.preset.Name], byStyle)
	plan.cells, plan.placement, err = applyMoves(plan.cells, plan.placement, moves, used, rules)
	return err
}

// applyMoves renames recipe nodes per the Spots step (R-K): every field of
// every cell that names the moved node names the new one instead. Every move
// is checked first (domain.RefuseSpotMoves): to one of its place's targets,
// no two places on one node, and no press and its attached place on a pairing
// no recipe set up. Whether the choreography still holds is the claim
// validator's question, asked by the save.
func applyMoves(cells []domain.FlowCell, placement, moves map[string]string, used []domain.NodeClaim, rules domain.SpotRules) ([]domain.FlowCell, map[string]string, error) {
	if err := domain.RefuseSpotMoves(cells, used, rules, moves); err != nil {
		return nil, nil, &MintRefusal{Reason: err.Error()}
	}
	rename := func(n string) string {
		if to, ok := moves[n]; ok && to != "" {
			return strings.TrimSpace(to)
		}
		return n
	}
	out := make([]domain.FlowCell, 0, len(cells))
	for _, c := range cells {
		c.CoreNodeName = rename(c.CoreNodeName)
		c.PairedCoreNode = rename(c.PairedCoreNode)
		c.SecondPairedCoreNode = rename(c.SecondPairedCoreNode)
		c.InboundStaging = rename(c.InboundStaging)
		c.OutboundStaging = rename(c.OutboundStaging)
		out = append(out, c)
	}
	moved := map[string]string{}
	for n, code := range placement {
		moved[rename(n)] = code
	}
	return out, moved, nil
}

// uniqueStyleName is the R-D name, with ` · <recipe>` on a collision with a
// live style's name, and a counter after that.
func uniqueStyleName(base, recipe string, styles []processes.Style) string {
	taken := map[string]bool{}
	for _, st := range styles {
		taken[st.Name] = true
	}
	if !taken[base] {
		return base
	}
	name := base + " · " + recipe
	for i := 2; taken[name]; i++ {
		name = fmt.Sprintf("%s · %s (%d)", base, recipe, i)
	}
	return name
}

func pictureIndex(order map[string]int, node string) int {
	if i, ok := order[node]; ok {
		return i
	}
	return len(order)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func wasWere(n int) string {
	if n == 1 {
		return "was"
	}
	return "were"
}

// MintStyle resolves or mints. A resolved style writes nothing; a minted one is
// created with its claims in one transaction through saveFlowTx, so the gate,
// the validator and the dedicated-spot check are the save's own.
func (e *Engine) MintStyle(processID int64, req MintRequest) (*MintResult, error) {
	if req.Source != domain.ClaimSourceHMI && req.Source != domain.ClaimSourceAdmin {
		return nil, fmt.Errorf("%w: got %q", ErrFlowSourceUnset, req.Source)
	}
	plan, err := e.planMint(processID, req)
	if err != nil {
		return nil, err
	}
	if plan.existing != nil {
		fp, err := e.FlowFingerprint(processID, plan.existing.ID)
		if err != nil {
			return nil, err
		}
		running := plan.process.ActiveStyleID != nil && *plan.process.ActiveStyleID == plan.existing.ID
		return &MintResult{StyleID: plan.existing.ID, StyleName: plan.existing.Name, Fingerprint: fp,
			Placement: plan.placement, Running: running}, nil
	}
	if _, err := e.db.GetActiveProcessChangeover(processID); err == nil {
		return nil, ErrChangeoverActive
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	claimCtx, err := e.claimContext(processID)
	if err != nil {
		return nil, err
	}
	stamp, err := e.mintStamp(processID)
	if err != nil {
		return nil, err
	}
	presetID, version := plan.preset.ID, plan.preset.Version
	var result *FlowSaveResult
	var cleared []processes.KeptSpot
	var styleID int64
	err = e.db.Transaction(func(tx *sql.Tx) error {
		id, err := processes.CreateStyle(tx, plan.name, "", processID)
		if err != nil {
			return fmt.Errorf("create style %s: %w", plan.name, err)
		}
		styleID = id
		fp, err := flowFingerprintOn(tx, processID, id)
		if err != nil {
			return err
		}
		result, cleared, err = e.saveFlowTx(tx, plan.process, processes.Style{ID: id, Name: plan.name, ProcessID: processID}, claimCtx, FlowSaveRequest{
			ToStyleID: id, Cells: plan.cells, Fingerprint: fp,
			Source: req.Source, CalledBy: req.CalledBy,
			SourcePresetID: &presetID, SourcePresetVersion: &version,
			stamp: stamp,
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	e.spotsCleared(cleared)
	return &MintResult{StyleID: styleID, StyleName: plan.name, Fingerprint: result.Fingerprint,
		Minted: true, Placement: plan.placement}, nil
}

// mintStamp is what a minted claim takes from its cell rather than from any
// other style: the process's containment ROUTE on produce slots, and a node's
// light-curtain requirement. Both are facts about the cell, not policy about a
// part — see DECISIONS-builder-pick-a-part (containment default taken without
// a ruling; curtain copied from the node).
func (e *Engine) mintStamp(processID int64) (func(*domain.NodeClaimInput), error) {
	dests, err := e.db.ListProduceContainmentDestinations()
	if err != nil {
		return nil, err
	}
	dest := dests[processID]
	live, err := processes.ListLiveClaimsByProcess(e.db.DB, processID)
	if err != nil {
		return nil, err
	}
	curtain := map[string]bool{}
	for _, c := range live {
		if c.CurtainRequired {
			curtain[c.CoreNodeName] = true
		}
	}
	return func(in *domain.NodeClaimInput) {
		if in.Role == protocol.ClaimRoleProduce && dest != "" {
			in.ContainmentDestination = dest
		}
		in.CurtainRequired = curtain[in.CoreNodeName]
	}, nil
}

// MintFacts is what a mint preview adds to a FlowPreview: the style the parts
// resolve to or would mint, its name, and where each part lands — the Go
// screen's sentence.
type MintFacts struct {
	StyleID   int64             `json:"style_id,omitempty"`
	StyleName string            `json:"style_name"`
	Minted    bool              `json:"minted"`
	Placement map[string]string `json:"placement"`
	Running   bool              `json:"running,omitempty"`
	// Cells is the flow the changeover goes to — the style's claims, or
	// the recipe with the parts on it — so the confirm sheet draws the press
	// it is about to make rather than describing it.
	Cells []domain.FlowCell `json:"cells,omitempty"`
}

// previewMint plans the mint and previews it (PreviewFlow with req.Mint set).
// Nothing is written: a resolved style is previewed as stored; a style to be
// minted is previewed as style 0, which planChangeoverFrom reads as "the style
// this changeover will create".
func (e *Engine) previewMint(ctx context.Context, processID int64, req FlowPreviewRequest) (*FlowPreview, error) {
	plan, err := e.planMint(processID, *req.Mint)
	if err != nil {
		return nil, err
	}
	facts := &MintFacts{StyleName: plan.name, Placement: plan.placement}
	pr := FlowPreviewRequest{Preflight: req.Preflight}
	if plan.existing != nil {
		facts.StyleID = plan.existing.ID
		facts.Running = plan.process.ActiveStyleID != nil && *plan.process.ActiveStyleID == plan.existing.ID
		claims, err := e.db.ListStyleNodeClaims(plan.existing.ID)
		if err != nil {
			return nil, err
		}
		pr.ToStyleID = plan.existing.ID
		for _, c := range claims {
			pr.Cells = append(pr.Cells, domain.Collapse(c))
		}
	} else {
		facts.Minted = true
		pr.Cells = plan.cells
	}
	facts.Cells = pr.Cells
	preview, err := e.PreviewFlow(ctx, processID, pr)
	if preview != nil {
		preview.Mint = facts
	}
	var noOrders *FlowNoOrdersError
	if errors.As(err, &noOrders) && noOrders.Preview != nil {
		noOrders.Preview.Mint = facts
	}
	return preview, err
}
