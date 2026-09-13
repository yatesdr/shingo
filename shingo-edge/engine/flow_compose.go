// flow_compose.go — the flow composer's engine verbs: preview a draft flow
// without writing, save it in one transaction under the fingerprint it was
// previewed with, and recompute that fingerprint for a start.
//
// The composer builds a flow in memory and must see the orders it would fire
// BEFORE anything is written — save-as-you-go was killed because a half-built
// style reaches Core's sourceability feed and every station's picker flaps
// NO PARTS. So the planner has a seam that takes claims (planChangeoverFrom),
// the preview and save sit on it, and a fingerprint of the stored rows the
// preview was planned over is what save and start check.

package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"shingoedge/domain"
	"shingoedge/domain/flowspec"
	"shingoedge/engine/changeover"
	"shingoedge/service"
	"shingoedge/store/processes"
)

// FlowPreviewRequest is a draft to plan.
//
// NO ReplaceAll. A request carries the WHOLE flow for its style — the picture
// IS the flow, so a target-style claim the picture does not show is a
// deletion — and that was already true of all six producers on this tree.
// The option existed as `false` on paper only, and it was the one path on
// which preview and save validated different sets: with a partial request the
// preview validated the carried claims and the save never touched them, so a
// draft could preview clean and save a flow nobody had previewed.
type FlowPreviewRequest struct {
	ToStyleID int64
	Cells     []domain.FlowCell
	// Preflight asks Core whether the draft's parts exist. Off by default:
	// this is a network call, the edit loop makes a preview every 400 ms, and
	// only the HMI's confirm sheet reads the answer. See flowPreflight.
	Preflight bool
}

// FlowPreflight is Core's answer about the draft's parts.
type FlowPreflight struct {
	// State is "ok", "missing" or "unchecked". Unchecked means Core could not
	// be asked — never an error for a preview, the picture stays live.
	State   string   `json:"state"`
	Missing []string `json:"missing"`
}

const (
	FlowPreflightOK        = "ok"
	FlowPreflightMissing   = "missing"
	FlowPreflightUnchecked = "unchecked"
)

// FlowPreview is what the composer renders: the orders that would fire, the
// findings that block, the nodes nothing can be planned for, Core's word on
// the parts, and the fingerprint of the stored rows this was planned over.
// The handler writes it verbatim.
type FlowPreview struct {
	Actions []changeover.PreviewAction `json:"actions"`
	// OrderCount is nil, and the field absent from the wire, when the target
	// is the style the press is RUNNING: that flow is not planned (owner
	// ruling R3, 2026-09-12), so there is no count to give. Absent and not
	// zero, because zero is a real and different answer — "this flow fires no
	// orders" is a blocked bar, and a running style whose saved changes land
	// at the next changeover is not blocked.
	OrderCount  *int                 `json:"order_count,omitempty"`
	Findings    []domain.NodeFinding `json:"findings"`
	Unresolved  []string             `json:"unresolved"`
	Preflight   FlowPreflight        `json:"preflight"`
	Fingerprint string               `json:"fingerprint"`
	// Running marks the preview of the running style: validated, fingerprinted
	// and not planned. The bar reads it rather than inferring it from a missing
	// count, which would also be true of a preview that failed to plan.
	Running bool `json:"running,omitempty"`
}

// Orders is the count for a preview that planned, and 0 for one that did not.
// Callers that need the distinction read OrderCount directly.
func (p *FlowPreview) Orders() int {
	if p == nil || p.OrderCount == nil {
		return 0
	}
	return *p.OrderCount
}

// FlowSaveRequest is a flow to write. CalledBy is the operator station's
// NAME, resolved by the handler from the request's station id; the client
// never sends it.
type FlowSaveRequest struct {
	ToStyleID   int64
	Cells       []domain.FlowCell
	Fingerprint string
	// Source and CalledBy are the claim's authorship, and they are the
	// HANDLER's to decide, never the body's. Both surfaces POST the same
	// route, so until a save could say which one it came from, every claim on
	// both plants read `hmi` with a station's name on it and the HMI's set-up
	// card could not say where a flow came from (owner ruling R1).
	//
	// domain.ClaimSourceAdmin for a save made in an authenticated session —
	// the desktop — with the session user as CalledBy; domain.ClaimSourceHMI
	// for one made without, with the station's name. Empty Source is refused
	// rather than defaulted: a caller that forgot is a caller writing the
	// wrong authorship, and silently picking one for them is how the two
	// surfaces became indistinguishable in the first place.
	Source   string
	CalledBy string
	// SourcePresetID / SourcePresetVersion are the preset an APPLY came from
	// (U10), and nil on every other save.
	//
	// POINTER-GATED, AND THAT IS THE WHOLE DESIGN. Expand speaks these two
	// columns only when they are set, so a hand edit of a preset-applied flow
	// leaves the provenance alone and the row keeps saying where its shape
	// came from — which is exactly what makes drift computable: the claim says
	// "I came from v2", the compare says whether it still looks like v2, and
	// the two answers are independent. A save that overwrote the provenance
	// with nothing would turn every manual edit into a lost lineage.
	SourcePresetID      *int64
	SourcePresetVersion *int
}

// FlowSaveResult is what a save returns: the NEW fingerprint (post-save),
// which start then carries, and the row counts. Written counts upserts; a
// locked cell whose claim already exists is not rewritten — the HMI did not
// author it, and stamping it hmi would say it did.
type FlowSaveResult struct {
	Fingerprint string `json:"fingerprint"`
	Written     int    `json:"written"`
	Deleted     int    `json:"deleted"`
}

var (
	// ErrFlowStale: the stored rows no longer match the fingerprint the
	// caller carries. Preview again.
	ErrFlowStale = errors.New("the flow changed since it was previewed — preview again")
	// ErrFlowComposerDisabled: the process has not been opened to the floor.
	ErrFlowComposerDisabled = errors.New("flow composer is not enabled for this process")

	// ErrFlowSourceUnset refuses a save whose caller did not say which surface
	// made it.
	//
	// IT IS REFUSED RATHER THAN DEFAULTED, and the reason is the trap this
	// round closed: MaterializeClaim reads an empty source as
	// domain.ClaimSourceAdmin (domain/flow.go:408). A caller who forgot would
	// therefore write "a person on the desktop did this" onto every claim it
	// touched — the exact wrong answer, silently, and the kind that only shows
	// up months later in a caption. The handler decides the source from the
	// authenticated caller; anything that reaches here without one is a bug in
	// a caller, not a default to guess at.
	ErrFlowSourceUnset = errors.New("a flow save must say which surface made it")
)

// FlowNoOrdersError is a preview that fires nothing. It carries the preview
// so the caller can still show the findings and the unresolved nodes — the
// reason there is nothing to fire is usually one of them.
type FlowNoOrdersError struct {
	Preview *FlowPreview
}

func (e *FlowNoOrdersError) Error() string {
	reason := "the flow fires no orders: nothing changes at any position"
	if p := e.Preview; p != nil {
		var parts []string
		for _, a := range p.Actions {
			if a.Error != "" {
				parts = append(parts, a.CoreNodeName+": "+a.Error)
			}
		}
		if len(p.Unresolved) > 0 {
			parts = append(parts, "no process node for "+strings.Join(p.Unresolved, ", "))
		}
		if len(parts) > 0 {
			reason = "the flow fires no orders: " + strings.Join(parts, "; ")
		}
	}
	return reason
}

// FlowValidationError is a save refused by the claim validator: nothing was
// written.
type FlowValidationError struct {
	Findings []domain.NodeFinding
}

func (e *FlowValidationError) Error() string {
	if len(e.Findings) == 0 {
		return "invalid flow"
	}
	return e.Findings[0].CoreNodeName + ": " + e.Findings[0].Message
}

// flowDraft is a request's cells expanded over the target style's stored
// claims: one entry per cell, and the stored claims the request did not
// mention, which are deletions.
type flowDraft struct {
	cells []flowDraftCell
	// removed are the stored claims the request left out. A request carries
	// the whole flow, so that is every one of them it did not name.
	removed []processes.NodeClaim
}

type flowDraftCell struct {
	cell  domain.FlowCell
	prior *processes.NodeClaim
	input domain.NodeClaimInput
	claim processes.NodeClaim
}

// claims is the target style as the draft would leave it — what the planner
// sees.
func (d flowDraft) claims() []processes.NodeClaim {
	out := make([]processes.NodeClaim, 0, len(d.cells))
	for _, c := range d.cells {
		out = append(out, c.claim)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CoreNodeName < out[j].CoreNodeName })
	return out
}

// inputs is every effective to-claim as a write, for the validator. It is the
// cells and nothing else: there are no carried claims, because a request that
// does not name a position is deleting it.
func (d flowDraft) inputs() []domain.NodeClaimInput {
	out := make([]domain.NodeClaimInput, 0, len(d.cells))
	for _, c := range d.cells {
		out = append(out, c.input)
	}
	return out
}

func draftFlow(toStyleID int64, stored []processes.NodeClaim, cells []domain.FlowCell,
	source, calledBy string, presetID *int64, presetVersion *int) flowDraft {
	byNode := make(map[string]*processes.NodeClaim, len(stored))
	for i := range stored {
		byNode[stored[i].CoreNodeName] = &stored[i]
	}
	var d flowDraft
	seen := map[string]bool{}
	for _, cell := range cells {
		cell.CoreNodeName = strings.TrimSpace(cell.CoreNodeName)
		prior := byNode[cell.CoreNodeName]
		in := domain.Expand(cell, prior, source, calledBy)
		in.StyleID = toStyleID
		// Provenance rides along when the caller has it, and is left NIL
		// otherwise so the store's pointer-gated update does not touch the
		// columns. See FlowSaveRequest.
		in.SourcePresetID, in.SourcePresetVersion = presetID, presetVersion
		d.cells = append(d.cells, flowDraftCell{cell: cell, prior: prior, input: in, claim: domain.MaterializeClaim(in, prior)})
		seen[cell.CoreNodeName] = true
	}
	// A stored claim the request does not name is a DELETION. The request is
	// the whole flow; see FlowPreviewRequest.
	for _, c := range stored {
		if !seen[c.CoreNodeName] {
			d.removed = append(d.removed, c)
		}
	}
	return d
}

// claimContext resolves this process's validation context once per request:
// the process, every process_node on the Edge, Core's node set and the plant
// map's points.
//
// THE ONE BUILDER, shared with www's claim doors through
// domain.NewClaimContextSet. There were two, and the second said in its own
// comment that it mirrored the first.
func (e *Engine) claimContext(processID int64) (domain.ClaimContextSet, error) {
	nodes, err := e.db.ListProcessNodes()
	if err != nil {
		// Could not look. The set stays unchecked and the validator skips the
		// membership and key-route checks rather than refusing a write on a
		// question it could not ask.
		return domain.ClaimContextSet{}, nil
	}
	in := domain.ClaimContextInput{
		StyleProcessID:   processID,
		Nodes:            make([]domain.ClaimContextNode, 0, len(nodes)),
		KnownScenePoints: e.ScenePointNames(),
	}
	for i := range nodes {
		in.Nodes = append(in.Nodes, domain.ClaimContextNode{
			CoreNodeName: nodes[i].CoreNodeName, ProcessID: nodes[i].ProcessID,
		})
	}
	if known := e.CoreNodes(); len(known) > 0 {
		in.KnownCoreNodes = make(map[string]bool, len(known))
		for name := range known {
			in.KnownCoreNodes[name] = true
		}
	}
	return domain.NewClaimContextSet(in), nil
}

// validateFlowInputs runs the save-time validator over every input and
// returns its ERRORS as node findings on the to side. Warnings are advice
// the composer cannot act on and do not block a save, so they are not
// findings here.
func validateFlowInputs(ctx domain.ClaimContextSet, inputs []domain.NodeClaimInput) []domain.NodeFinding {
	var out []domain.NodeFinding
	for _, in := range inputs {
		for _, f := range domain.ValidateNodeClaim(in, ctx.ForNode(in.CoreNodeName)) {
			if f.Severity != domain.SeverityError {
				continue
			}
			out = append(out, domain.NodeFinding{
				CoreNodeName: in.CoreNodeName, Side: flowspec.SideTo, Field: flowspec.Field(f.Field),
				Severity: domain.SeverityError, Message: f.Message,
			})
		}
	}
	return out
}

// sortFindings orders findings by node, side, field, message so two runs
// over the same claims render the same list.
func sortFindings(findings []domain.NodeFinding) []domain.NodeFinding {
	if findings == nil {
		return []domain.NodeFinding{}
	}
	sort.SliceStable(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		if a.CoreNodeName != b.CoreNodeName {
			return a.CoreNodeName < b.CoreNodeName
		}
		if a.Side != b.Side {
			return a.Side < b.Side
		}
		if a.Field != b.Field {
			return a.Field < b.Field
		}
		return a.Message < b.Message
	})
	return findings
}

// flowPreflight asks Core about the draft's parts. Core unavailable, or a
// failed call, is UNCHECKED — the preview keeps the picture live and says
// the inventory was not checked, rather than failing the whole preview or
// pretending it looked.
//
// ASKED ONCE, AT THE CONFIRM SHEET, NOT ON THE EDIT LOOP (owner, 2026-09-13).
// It used to run on every preview — one Core HTTP call behind a 400 ms
// debounce, so a dragged slider was a call a keystroke — for a field only the
// HMI's confirm sheet reads and the desktop never reads at all. Now
// PreviewFlow takes it only when the caller asks (?preflight=1), which is the
// one request the confirm sheet makes; every other preview returns
// FlowPreflightUnchecked, which is the honest state and already existed.
//
// The context is the REQUEST's, so an operator who leaves the screen takes
// the Core call with them instead of leaving it to time out.
func (e *Engine) flowPreflight(ctx context.Context, claims []processes.NodeClaim) FlowPreflight {
	unchecked := FlowPreflight{State: FlowPreflightUnchecked, Missing: []string{}}
	if e.preflightChecker == nil || e.coreClient == nil || !e.coreClient.Available() {
		return unchecked
	}
	missing, err := e.preflightChecker.PreflightPayloads(ctx, service.PayloadCodesOf(claims))
	if err != nil {
		e.logFn("flow preview: inventory not checked: %v", err)
		return unchecked
	}
	if len(missing) == 0 {
		return FlowPreflight{State: FlowPreflightOK, Missing: []string{}}
	}
	sort.Strings(missing)
	return FlowPreflight{State: FlowPreflightMissing, Missing: missing}
}

// PreviewFlow plans a draft flow for the target style exactly as a start
// would plan the saved one, and writes nothing: no rows, no cancelled
// orders, no post-cutover flag cleared, no outbox row.
//
// Returns *FlowNoOrdersError (with the preview attached) when the plan fires
// nothing, ErrStyleAlreadyRunning / ErrChangeoverActive when the seam's gates
// refuse — the same words the desktop preview has always used.
func (e *Engine) PreviewFlow(ctx context.Context, processID int64, req FlowPreviewRequest) (*FlowPreview, error) {
	// ONE READ OF EACH THING. GetProcess ran three times per preview (here,
	// inside the fingerprint, and again inside the planner) and the process
	// nodes three times; the pull snapshot was walked twice. They are all
	// hoisted here and handed down.
	process, err := e.db.GetProcess(processID)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var fromClaims []processes.NodeClaim
	if process.ActiveStyleID != nil {
		if fromClaims, err = e.db.ListStyleNodeClaims(*process.ActiveStyleID); err != nil {
			return nil, fmt.Errorf("list from-style claims: %w", err)
		}
	}
	stored, err := e.db.ListStyleNodeClaims(req.ToStyleID)
	if err != nil {
		return nil, fmt.Errorf("list to-style claims: %w", err)
	}
	// The draft is stamped as the station would stamp it; the stamp is not
	// planned over and not fingerprinted, so any name will do here.
	// A preview writes nothing, so it carries no provenance: the columns are
	// not in the fingerprint (flow_fingerprint.go's column list), so a preview
	// and the save that follows it agree either way.
	draft := draftFlow(req.ToStyleID, stored, req.Cells, domain.ClaimSourceHMI, "", nil, nil)
	toClaims := draft.claims()

	fingerprint, err := domain.FlowFingerprint(processID, process.ActiveStyleID, fromClaims, req.ToStyleID, stored)
	if err != nil {
		return nil, err
	}
	claimCtx, err := e.claimContext(processID)
	if err != nil {
		return nil, err
	}
	findings := validateFlowInputs(claimCtx, draft.inputs())
	findings = append(findings, domain.ValidateFlowPartsPlaced(stored, toClaims)...)

	// THE RUNNING STYLE IS NOT PLANNED (owner ruling R3, 2026-09-12).
	//
	// A save to the style on the press is allowed — it lands at the next
	// changeover — but there is no changeover to plan: planChangeoverFrom
	// refuses a target that is already running, and rightly, because a
	// changeover TO the running style is nonsense. So this returns what a
	// preview of the running style CAN honestly answer — the validator's
	// findings and the fingerprint the save will be judged against — and no
	// count, no actions, no preflight over material nobody is about to move.
	//
	// The readiness check is skipped with the plan for the same reason: it
	// answers "which node would the planner refuse", and no planner is going
	// to be asked.
	if process.ActiveStyleID != nil && *process.ActiveStyleID == req.ToStyleID {
		return &FlowPreview{
			Actions:     []changeover.PreviewAction{},
			Findings:    sortFindings(findings),
			Unresolved:  []string{},
			Preflight:   FlowPreflight{State: FlowPreflightUnchecked, Missing: []string{}},
			Fingerprint: fingerprint,
			Running:     true,
		}, nil
	}

	plan, err := e.planChangeoverFrom(processID, req.ToStyleID, fromClaims, toClaims, false)
	if err != nil {
		return nil, err
	}
	orderPlan, skipped := BuildChangeoverPlanReporting(plan.diffs, plan.nodes, e.cfg.Web.AutoConfirm, e.activePullSnapshot(plan.nodes), plan.tooling)

	findings = append(findings, domain.ValidateFlowChangeoverReadiness(fromClaims, toClaims)...)

	count := orderPlan.OrderCount()
	preview := &FlowPreview{
		Actions:     make([]changeover.PreviewAction, 0, len(orderPlan.Actions)),
		OrderCount:  &count,
		Findings:    sortFindings(findings),
		Unresolved:  unionNames(plan.unresolvedParticipants, skipped),
		Preflight:   FlowPreflight{State: FlowPreflightUnchecked, Missing: []string{}},
		Fingerprint: fingerprint,
	}
	// Core is asked only when the caller says it is about to act on the
	// answer — see flowPreflight.
	if req.Preflight {
		preview.Preflight = e.flowPreflight(ctx, toClaims)
	}
	for _, a := range orderPlan.Actions {
		preview.Actions = append(preview.Actions, changeover.ToPreviewAction(a))
	}
	if count == 0 {
		return preview, &FlowNoOrdersError{Preview: preview}
	}
	return preview, nil
}

// unionNames is a ∪ b in order, deduped, never nil.
func unionNames(a, b []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, list := range [][]string{a, b} {
		for _, n := range list {
			if n == "" || seen[n] {
				continue
			}
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// FlowFingerprint is the fingerprint of the stored rows a preview of
// toStyleID would be planned over right now. Recomputed on every check,
// never stored.
func (e *Engine) FlowFingerprint(processID, toStyleID int64) (string, error) {
	return flowFingerprintOn(e.db.DB, processID, toStyleID)
}

func flowFingerprintOn(db processes.DBTX, processID, toStyleID int64) (string, error) {
	_, _, fp, err := flowRowsAndFingerprint(db, processID, toStyleID)
	return fp, err
}

// flowRowsAndFingerprint reads the rows a fingerprint is over and returns
// both — the claims AND the hash, from one pass.
//
// THE CALLER NEEDS THE ROWS TOO. The save read the to-side claims to compute
// the fingerprint, threw them away, and immediately re-read the same rows to
// build the draft; and then, after the transaction had committed, read all of
// them a third time to report the new fingerprint. Returning what was already
// read is the whole fix.
func flowRowsAndFingerprint(db processes.DBTX, processID, toStyleID int64) (from, to []processes.NodeClaim, fingerprint string, err error) {
	process, err := processes.Get(db, processID)
	if err != nil {
		return nil, nil, "", err
	}
	if process.ActiveStyleID != nil {
		if from, err = processes.ListClaims(db, *process.ActiveStyleID); err != nil {
			return nil, nil, "", fmt.Errorf("list from-style claims: %w", err)
		}
	}
	if to, err = processes.ListClaims(db, toStyleID); err != nil {
		return nil, nil, "", fmt.Errorf("list to-style claims: %w", err)
	}
	fingerprint, err = domain.FlowFingerprint(processID, process.ActiveStyleID, from, toStyleID, to)
	if err != nil {
		return nil, nil, "", err
	}
	return from, to, fingerprint, nil
}

// ErrRunningPositionMove refuses the one mid-run edit the runtime cannot
// follow: moving the RUNNING style off a position while its bin is on it.
// THE SENTINEL LIVES IN domain, where the store can raise it. This alias is
// what www's status mapping and the engine's own callers already match on.
var ErrRunningPositionMove = domain.ErrRunningPositionMove

// refuseRunningPositionMove is owner ruling R3's single gate (2026-09-12).
//
// SOURCES, DESTINATIONS AND ROUTES SAVE FREELY MID-RUN, and take effect on the
// next trip. Every runtime reader of the running style's flow resolves its
// claim at the moment it needs one, by (active_style_id, core_node_name), and
// reads the field off that row: the refill sweep
// (demand_reconciler.go:123 -> operator_helpers.go:103), the order manager's
// payload and SEER routing (orders/manager.go:57), the PLC tick's decrement
// (wiring_counter_delta.go:60), the delivery and completion handlers, and the
// next changeover's evacuation steps (material_orders.go:640 off
// fromClaim.OutboundDestination). None of them caches, so a changed
// inbound_source, outbound_destination or key_route is simply what the next
// trip uses. That is the flexibility the ruling asked for.
//
// A POSITION CHANGE IS NOT LIKE THE OTHERS, because every one of those readers
// is keyed by NODE NAME. Moving the running style from PLN_01 to PLN_03 sends
// PLN_01's stored claim through DeleteClaim (draftFlow's `removed` list) and
// writes a new one under PLN_03, and then:
//
//   - PLN_01, which physically holds the bin, has no active claim. The level
//     sweep skips it (demand_reconciler.go:124), so it is never refilled; the
//     PLC tick skips it (wiring_counter_delta.go:61), so the parts made off
//     that bin stop counting; every delivered/completed/departed handler
//     short-circuits on the same nil.
//   - PLN_03, which holds nothing, is swept: its cached count is whatever a
//     position that has never run carries, so it is below any reorder point
//     and a supply order is asked for to an empty position.
//   - The next changeover away diffs by node name (changeover.go:34-42), so
//     PLN_03 is a Drop or a Swap — an evacuation trip to a position with
//     nothing on it — and PLN_01 appears in neither map, so it is never
//     evacuated and its bin is stranded. Where the incoming style DOES claim
//     PLN_01 it is a SituationAdd, and new material is delivered onto a
//     position that still has the old part's bin on it.
//
// So the move is refused BY NAME, and only the move: this gate fires on the
// set of positions, never on what is written to one.
func refuseRunningPositionMove(styleName string, stored []processes.NodeClaim, draft flowDraft) error {
	was := map[string]bool{}
	for _, c := range stored {
		was[c.CoreNodeName] = true
	}
	now := map[string]bool{}
	for _, c := range draft.cells {
		now[c.cell.CoreNodeName] = true
	}
	var leaving, arriving []string
	for n := range was {
		if !now[n] {
			leaving = append(leaving, n)
		}
	}
	for n := range now {
		if !was[n] {
			arriving = append(arriving, n)
		}
	}
	if len(leaving) == 0 && len(arriving) == 0 {
		return nil
	}
	sort.Strings(leaving)
	sort.Strings(arriving)
	switch {
	case len(leaving) > 0 && len(arriving) > 0:
		return fmt.Errorf("%w: %s is running on %s — move it to %s after the next changeover",
			ErrRunningPositionMove, styleName, strings.Join(leaving, ", "), strings.Join(arriving, ", "))
	case len(leaving) > 0:
		return fmt.Errorf("%w: %s is running on %s — take that position off the flow after the next changeover",
			ErrRunningPositionMove, styleName, strings.Join(leaving, ", "))
	default:
		return fmt.Errorf("%w: %s is running — add %s to the flow after the next changeover",
			ErrRunningPositionMove, styleName, strings.Join(arriving, ", "))
	}
}

// SaveFlow writes a flow to the target style in ONE transaction.
//
// Refused before anything is written: the process's flow composer is off
// (ErrFlowComposerDisabled), a changeover is active (the seam's words), the
// target is the RUNNING style and the draft moves a position
// (ErrRunningPositionMove — see refuseRunningPositionMove for the readers that
// makes it unsafe), the recomputed fingerprint — re-read inside the
// transaction — differs from the request's (ErrFlowStale), or any cell fails
// ValidateNodeClaim (*FlowValidationError). A refusal has zero side effects:
// no row, no updated_at, no outbox row.
//
// SAVING THE RUNNING STYLE IS ALLOWED (owner ruling R3, 2026-09-12). It writes
// the rows and starts nothing — no changeover row, no orders, post_cutover
// untouched — and the next trip the runtime plans reads them. What it may not
// do is move the style off a position while its bin is on it.
//
// Every cell is written through UpsertClaim with the request's Source and
// CalledBy — the desktop's session user as admin, or the station's name as hmi
// (owner ruling R1); every stored claim the request left out goes through
// DeleteClaim (retire semantics apply). A locked cell whose
// claim exists is left exactly as it is. The caller publishes the process's
// claims once after this returns.
func (e *Engine) SaveFlow(processID int64, req FlowSaveRequest) (*FlowSaveResult, error) {
	if req.Source != domain.ClaimSourceHMI && req.Source != domain.ClaimSourceAdmin {
		return nil, fmt.Errorf("%w: got %q", ErrFlowSourceUnset, req.Source)
	}
	process, err := e.db.GetProcess(processID)
	if err != nil {
		return nil, err
	}
	// THE GATE IS OPERATORS-ONLY (owner, 2026-09-13). flow_composer_enabled is
	// the switch that opens this press TO THE FLOOR, and its refusal sentence
	// says so. Applying it to the desktop as well made the PLAN's own rollout
	// impossible — set the flows up on the desktop, then open the gate — and
	// gave an engineer a 403 about operators on a screen whose Save button was
	// enabled. The desktop had unrestricted claim writes on main; this route
	// is not where that changes.
	if req.Source == domain.ClaimSourceHMI && !process.FlowComposerEnabled {
		return nil, ErrFlowComposerDisabled
	}
	if _, err := e.db.GetActiveProcessChangeover(processID); err == nil {
		return nil, ErrChangeoverActive
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	style, err := e.db.GetStyle(req.ToStyleID)
	if err != nil {
		return nil, err
	}
	if style.ProcessID != processID {
		return nil, fmt.Errorf("target style %d does not belong to process %d", req.ToStyleID, processID)
	}
	// Read before the transaction: on a one-connection store a read on the
	// pool from inside the tx would wait on the tx itself.
	claimCtx, err := e.claimContext(processID)
	if err != nil {
		return nil, err
	}

	// WHAT THE TRANSACTION HOLDS, AND WHY ONLY THIS. The store is pinned to
	// ONE connection, so every board's poll queues behind whatever this holds.
	// It holds the claims reads and the writes and nothing else:
	//
	//   - The claims reads stay INSIDE because they are the staleness
	//     guarantee. On a one-connection pool another goroutine can still
	//     interleave between a read taken outside and the BEGIN that follows
	//     it, and a fingerprint compared against rows that moved in that gap
	//     is the check not happening.
	//   - The ROUTING read is gone entirely: it left the fingerprint (it is an
	//     offer list, not a flow), and it was the most expensive query in the
	//     feature — an unindexable correlated COUNT(DISTINCT) — sitting inside
	//     the exclusive hold, 37% of it.
	//   - The second read of the to-side claims is gone: the fingerprint pass
	//     had already read exactly those rows.
	//   - The NEW fingerprint is computed from the rows the transaction holds
	//     and returned, rather than re-reading the whole flow after commit.
	var result FlowSaveResult
	err = e.db.Transaction(func(tx *sql.Tx) error {
		fromClaims, stored, current, err := flowRowsAndFingerprint(tx, processID, req.ToStyleID)
		if err != nil {
			return err
		}
		if current != req.Fingerprint {
			return ErrFlowStale
		}
		draft := draftFlow(req.ToStyleID, stored, req.Cells, req.Source, req.CalledBy,
			req.SourcePresetID, req.SourcePresetVersion)
		if running := process.ActiveStyleID != nil && *process.ActiveStyleID == req.ToStyleID; running {
			if err := refuseRunningPositionMove(style.Name, stored, draft); err != nil {
				return err
			}
		}
		cellInputs := make([]domain.NodeClaimInput, 0, len(draft.cells))
		for _, c := range draft.cells {
			cellInputs = append(cellInputs, c.input)
		}
		if findings := validateFlowInputs(claimCtx, cellInputs); len(findings) > 0 {
			return &FlowValidationError{Findings: sortFindings(findings)}
		}
		for _, c := range draft.cells {
			if _, err := processes.UpsertClaim(tx, c.input); err != nil {
				return fmt.Errorf("%s: %w", c.cell.CoreNodeName, err)
			}
			result.Written++
		}
		for _, c := range draft.removed {
			if err := processes.DeleteClaim(tx, c.ID); err != nil {
				return fmt.Errorf("%s: %w", c.CoreNodeName, err)
			}
			result.Deleted++
		}
		// The new fingerprint, computed INSIDE the transaction and returned.
		//
		// ONE READ, NOT FOUR. It used to be recomputed after the commit, which
		// re-read the process, both sides' claims and the routing set on the
		// pool the boards were queued on. Here the process and the from-side
		// are already in hand, so only the side that was just written is read
		// back.
		//
		// READ BACK RATHER THAN DERIVED FROM THE DRAFT, and this is the part
		// that has to be a read: the fingerprint covers id and sequence, and
		// both are assigned by the store on insert. A fingerprint built from
		// the draft would carry id 0 for every new row, match nothing a later
		// check computes, and turn the caller's next save into ErrFlowStale.
		written, err := processes.ListClaims(tx, req.ToStyleID)
		if err != nil {
			return fmt.Errorf("read back to-style claims: %w", err)
		}
		// A save cannot move the running style's claims unless the running
		// style IS the target, and then the side just read is both sides.
		if process.ActiveStyleID != nil && *process.ActiveStyleID == req.ToStyleID {
			fromClaims = written
		}
		result.Fingerprint, err = domain.FlowFingerprint(processID, process.ActiveStyleID, fromClaims, req.ToStyleID, written)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}
