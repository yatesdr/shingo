// claims.go — style/node-claim persistence inside the processes aggregate.
//
// Phase 6.0c folded shingo-edge/store/claims/ into store/processes/.
// Claims declare which core nodes a style needs material from; they're
// part of the process domain cluster (style → claims → core nodes).
// Function names carry the Claim suffix to disambiguate from the sibling
// Style/Process/Changeover/Node functions in this package.

package processes

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/domain/flowspec"
	"shingoedge/store/internal/capacity"
	"shingoedge/store/internal/helpers"
)

// NodeClaim and NodeClaimInput are the claim-aggregate data types.
//
// NodeClaim declares that a style needs a specific core node with a
// given payload and role. Two roles are supported:
//   - "consume":   system delivers full bins and removes empties
//   - "produce":   system delivers empty bins and removes filled ones
//
// (The legacy "changeover" role was removed during the UI consistency
// refactor; changeovers are now driven by swap_mode + EvacuateOnChangeover.)
//
// SwapMode controls the choreography:
//   - "simple":      RETIRED as a configurable claim mode — UpsertClaim no
//     longer accepts it. Survives only as a runtime CycleMode descriptor for
//     the node-empty downgrade (see protocol.SwapModeSimple).
//   - "sequential":  backfill while current bin is in transit
//   - "single_robot": inbound + outbound staging for single-robot swap
//   - "two_robot":   dual-robot swap with inbound staging
//   - "two_robot_press_index": dual-robot press-index swap (R1 carries full
//     out + replacement in; R2 indexes B→A)
//   - "manual_swap": operator-driven forklift swap with multi-order queue
//
// Routing fields follow a directional convention:
//
//	InboundSource → InboundStaging → CoreNodeName → OutboundStaging → OutboundDestination
//
// InboundSource is where inbound material is picked up FROM.
// OutboundDestination is where outbound material is dropped off TO.
//
// The structs (and the NodeClaim.AllowedPayloads method) live in
// shingoedge/domain (Stage 2A.2); these aliases keep the unprefixed
// processes.NodeClaim / processes.NodeClaimInput names used by every
// scan helper, Upsert call site, and the outer store/ re-exports.
type (
	NodeClaim      = domain.NodeClaim
	NodeClaimInput = domain.NodeClaimInput
)

// claimSelect names the claim columns in scan order. uop_capacity is NOT among
// them: the column is dead and the value is resolved from the payload catalog
// on every read — see capacity.SQL for why it is resolved here rather
// than by each of the readers.
var claimSelect = `id, style_id, core_node_name, role, swap_mode, payload_code,
	` + capacity.SQL("style_node_claims") + `, reorder_point, reorder_point_source, auto_reorder, inbound_staging, outbound_staging,
	inbound_source, outbound_destination, allowed_payload_codes, auto_request_payload,
	keep_staged, evacuate_on_changeover, paired_core_node, auto_confirm, sequence,
	lineside_soft_threshold, second_paired_core_node,
	reuse_compatible_bins, auto_push, below_reorder_since, created_at,
	changeover_evac_nodes, changeover_evac_destination,
	index_robot_supplies, key_route, key_task, changeover_carryover_disposition,
	source, called_by, updated_at, retired_at, source_preset_id, source_preset_version`

// liveClaims is the WHERE fragment for "claims that still drive anything":
// every LIST and by-(style, node) read applies it, so a retired claim reads
// as absent exactly the way a deleted one did. GetClaim (by id) deliberately
// does not — it resolves an id somebody already holds, which is how a
// changeover's history label keeps rendering after the claim is gone.
const liveClaims = ` retired_at IS NULL`

func scanNodeClaim(scanner interface{ Scan(...any) error }) (NodeClaim, error) {
	var c NodeClaim
	var resolvedCapacity int
	var createdAt, allowedJSON, evacNodesJSON, keyRouteJSON string
	var belowSince, updatedAt, retiredAt sql.NullString
	var presetID, presetVersion sql.NullInt64
	if err := scanner.Scan(&c.ID, &c.StyleID, &c.CoreNodeName, &c.Role, &c.SwapMode, &c.PayloadCode,
		&resolvedCapacity, &c.ReorderPoint, &c.ReorderPointSource, &c.AutoReorder, &c.InboundStaging, &c.OutboundStaging,
		&c.InboundSource, &c.OutboundDestination, &allowedJSON, &c.AutoRequestPayload,
		&c.KeepStaged, &c.EvacuateOnChangeover, &c.PairedCoreNode, &c.AutoConfirm, &c.Sequence,
		&c.LinesideSoftThreshold, &c.SecondPairedCoreNode,
		&c.ReuseCompatibleBins, &c.AutoPush, &belowSince, &createdAt,
		&evacNodesJSON, &c.ChangeoverEvacDestination,
		&c.IndexRobotSupplies, &keyRouteJSON, &c.KeyTask, &c.ChangeoverCarryoverDisposition,
		&c.Source, &c.CalledBy, &updatedAt, &retiredAt, &presetID, &presetVersion); err != nil {
		return c, err
	}
	c.UOPCapacity = capacity.Resolved(resolvedCapacity, c.PayloadCode)
	c.UpdatedAt = helpers.ScanTimePtr(updatedAt)
	c.RetiredAt = helpers.ScanTimePtr(retiredAt)
	if presetID.Valid {
		v := presetID.Int64
		c.SourcePresetID = &v
	}
	if presetVersion.Valid {
		v := int(presetVersion.Int64)
		c.SourcePresetVersion = &v
	}
	// NULL means "not below", which is the ordinary state — a zero time would
	// read as an episode that opened at the epoch.
	if belowSince.Valid && belowSince.String != "" {
		if t := helpers.ScanTime(belowSince.String); !t.IsZero() {
			c.BelowReorderSince = &t
		}
	}
	c.CreatedAt = helpers.ScanTime(createdAt)
	if allowedJSON != "" {
		_ = json.Unmarshal([]byte(allowedJSON), &c.AllowedPayloadCodes)
	}
	if evacNodesJSON != "" {
		_ = json.Unmarshal([]byte(evacNodesJSON), &c.ChangeoverEvacNodes)
	}
	if keyRouteJSON != "" {
		_ = json.Unmarshal([]byte(keyRouteJSON), &c.KeyRoute)
	}
	return c, nil
}

// ListClaims returns every claim for a style.
func ListClaims(db DBTX, styleID int64) ([]NodeClaim, error) {
	rows, err := db.Query(`SELECT `+claimSelect+`
		FROM style_node_claims WHERE style_id=? AND`+liveClaims+` ORDER BY sequence, core_node_name`, styleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NodeClaim
	for rows.Next() {
		c, err := scanNodeClaim(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListLiveClaimsByProcess returns every claim on every LIVE style of one
// process in ONE query, ordered by style then (sequence, core_node_name) —
// the same per-style order ListClaims gives, so a caller grouping by StyleID
// sees each style's claims exactly as ListClaims would have listed them.
//
// It exists so a per-process reader (the plant-claims publisher) does not
// issue one ListClaims per style. On a store pinned to one connection that
// loop was 1 + styles queries per process, and Press 4 and Press 6 at
// Springfield are headed for 40-90 styles each. Same shape as
// PayloadsForManualSwapNodes in walk.go, which replaced a per-style walk
// after a 44-second board build.
//
// The style filter is a semi-join rather than a JOIN so claimSelect and
// scanNodeClaim are reused verbatim: both tables carry id and created_at, and
// a hand-qualified copy of the column list is exactly the drift walk.go's
// comment warns about. The subquery applies liveStyles — a retired style's
// claims must not reach Core, exactly as ListStylesByProcess would not have
// visited them — and the outer WHERE applies liveClaims, so a retired claim
// (one changeover history still points at) reads as absent here exactly as
// it does in ListClaims.
func ListLiveClaimsByProcess(db *sql.DB, processID int64) ([]NodeClaim, error) {
	rows, err := db.Query(`SELECT `+claimSelect+`
		FROM style_node_claims
		WHERE style_id IN (SELECT id FROM styles WHERE process_id = ? AND`+liveStyles+`)
		  AND`+liveClaims+`
		ORDER BY style_id, sequence, core_node_name`, processID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NodeClaim
	for rows.Next() {
		c, err := scanNodeClaim(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetClaim returns a single claim by id.
func GetClaim(db *sql.DB, id int64) (*NodeClaim, error) {
	c, err := scanNodeClaim(db.QueryRow(`SELECT `+claimSelect+`
		FROM style_node_claims WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ClaimForLinesidePayload returns the claim at a node whose payload matches the
// carrier standing there — the answer to "where does THIS bin belong", asked
// without reference to which style the process is currently on.
//
// EXACTLY ONE MATCH, or nothing. A node several styles claim for the same
// payload does not have one home for it, and guessing between them would put a
// carrier somewhere plausible and wrong. Blank payload returns nothing: an
// empty carrier's destination is not a payload question.
//
// LIVE CLAIMS ONLY. A retired row is a claim the flow no longer has, and a
// composer save retires rows routinely — every replace_all that drops a
// position does. Without the predicate a retired claim could be the "exactly
// one match" that answers where a bin belongs, and the evacuation resolver
// would send a carrier to a position the current flow does not use. Worse, a
// live claim and its own retired predecessor at the same node would count as
// two matches and the resolver would answer nothing at all.
func ClaimForLinesidePayload(db *sql.DB, coreNodeName, payloadCode string) (*NodeClaim, error) {
	if coreNodeName == "" || payloadCode == "" {
		return nil, nil
	}
	rows, err := db.Query(`SELECT `+claimSelect+`
		FROM style_node_claims WHERE core_node_name=? AND payload_code=? AND`+liveClaims, coreNodeName, payloadCode)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var found []NodeClaim
	for rows.Next() {
		c, serr := scanNodeClaim(rows)
		if serr != nil {
			return nil, serr
		}
		found = append(found, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(found) != 1 {
		return nil, nil
	}
	return &found[0], nil
}

// GetClaimByNode returns a claim by its (style_id, core_node_name) pair.
func GetClaimByNode(db *sql.DB, styleID int64, coreNodeName string) (*NodeClaim, error) {
	c, err := scanNodeClaim(db.QueryRow(`SELECT `+claimSelect+`
		FROM style_node_claims WHERE style_id=? AND core_node_name=? AND`+liveClaims, styleID, coreNodeName))
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ClaimsByNodeForStyles is GetClaimByNode for a whole set of styles at once:
// one query, keyed [style_id][core_node_name].
//
// THE KEY IS UNIQUE IN THE TABLE, which is what makes the map a faithful
// stand-in rather than a guess about which row a point read would have
// returned. style_node_claims carries UNIQUE(style_id, core_node_name), so
// there is exactly one row per key and GetClaimByNode's unordered QueryRow had
// only ever one row to pick from. If that constraint is ever relaxed, this map
// and that QueryRow begin disagreeing silently and both need revisiting.
//
// It exists for the three per-node walkers — the level sweep, the parked-ticks
// monitor and the counter tick — each of which resolved the claim one node at a
// time on a store pinned to one connection. Same liveClaims predicate as
// GetClaimByNode, so a retired claim is absent here exactly as it reads absent
// there. A style with no live claims is simply missing from the outer map,
// which reads the same as GetClaimByNode's sql.ErrNoRows.
func ClaimsByNodeForStyles(db *sql.DB, styleIDs []int64) (map[int64]map[string]*NodeClaim, error) {
	out := map[int64]map[string]*NodeClaim{}
	if len(styleIDs) == 0 {
		return out, nil
	}
	seen := make(map[int64]bool, len(styleIDs))
	args := make([]any, 0, len(styleIDs))
	var placeholders strings.Builder
	for _, id := range styleIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		if placeholders.Len() > 0 {
			placeholders.WriteByte(',')
		}
		placeholders.WriteByte('?')
		args = append(args, id)
	}
	rows, err := db.Query(`SELECT `+claimSelect+`
		FROM style_node_claims WHERE style_id IN (`+placeholders.String()+`) AND`+liveClaims, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		c, err := scanNodeClaim(rows)
		if err != nil {
			return nil, err
		}
		claim := c
		byNode := out[claim.StyleID]
		if byNode == nil {
			byNode = map[string]*NodeClaim{}
			out[claim.StyleID] = byNode
		}
		byNode[claim.CoreNodeName] = &claim
	}
	return out, rows.Err()
}

// IsPairedOnDeckNode reports whether coreNodeName is used as a paired /
// on-deck (back) position by any claim of a style in the given process — i.e.
// a two_robot_press_index PairedCoreNode or SecondPairedCoreNode. Such
// positions hold ONLY an empty carrier waiting to be indexed onto the core
// (front) position, so a part-number stamp on one violates the invariant that
// hung the Hopkinsville press-index swap (2026-07-23). A blank coreNodeName
// never matches (blank paired fields on non-press-index claims must not
// false-positive).
//
// RETIRED styles are excluded. This decides live behaviour rather than
// rendering text: a claim belonging to a style nobody can run any more must not
// keep a node marked as an on-deck position, because that would go on refusing
// a part-number stamp on a node the plant has moved on from.
func IsPairedOnDeckNode(db *sql.DB, processID int64, coreNodeName string) (bool, error) {
	name := strings.TrimSpace(coreNodeName)
	if name == "" {
		return false, nil
	}
	var exists int
	err := db.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM style_node_claims c
			JOIN styles s ON c.style_id = s.id
			WHERE s.process_id = ?
			  AND s.deleted_at IS NULL
			  AND c.retired_at IS NULL
			  AND (c.paired_core_node = ? OR c.second_paired_core_node = ?)
		)`, processID, name, name).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists == 1, nil
}

// UpsertClaim inserts or updates a claim and returns the row id. Validates
// role/swap_mode invariants (manual_swap claims must auto-confirm and
// must declare an outbound destination).
func UpsertClaim(db DBTX, in NodeClaimInput) (int64, error) {
	// Defense-in-depth: API ingress (apiUpsertStyleNodeClaim) trims
	// these. Trim again here so a non-API caller can't bypass it.
	// Internal write path; silent trim, no warning log.
	in.CoreNodeName = strings.TrimSpace(in.CoreNodeName)
	in.PairedCoreNode = strings.TrimSpace(in.PairedCoreNode)
	in.SecondPairedCoreNode = strings.TrimSpace(in.SecondPairedCoreNode)

	if in.Role != protocol.ClaimRoleProduce {
		in.Role = protocol.ClaimRoleConsume
	}
	// swap_mode is required — fail loud on blank rather than silently pick a
	// mode. Both UI writers always send one: the claim editor defaults new claims
	// to single_robot, and the compare grid echoes the claim's stored value back
	// verbatim (www/static/js/pages/processes.js claimToBody). So a blank here is
	// a non-UI caller — an import, or a stale API poke.
	// No mode is a safe default: two_robot needs inbound staging and
	// single_robot needs inbound+outbound staging, so any default would only
	// trade a mode error for a more misleading staging error.
	if in.SwapMode == "" {
		return 0, fmt.Errorf("%w: swap_mode is required", protocol.ErrInvalidSwapMode)
	}
	// SwapMode allowlist, keyed on protocol.ConfigurableSwapModes() — the set of
	// values that may be PERSISTED. It is deliberately NOT the editor dropdown:
	// the dropdown carries a hidden "simple" for rendering old rows, and
	// www/processes_enum_drift_test.go asserts that difference rather than
	// forbidding it. The retired "simple" is absent here — it survives only as a
	// runtime CycleMode descriptor, never a persisted claim mode. So is
	// manual_swap, and for the ownership reason rather than a rendering one: a
	// loader is Core-owned topology served by SynthClaim, so a stored loader
	// claim is a second authority and there is now nothing for one to be about.
	// "press_position" (the per-position fan-out marker) is in-memory only and
	// must never persist; the allowlist also rejects typos and stale import
	// values.
	if !slices.Contains(protocol.ConfigurableSwapModes(), in.SwapMode) {
		return 0, fmt.Errorf("%w: %q is not a configurable swap_mode", protocol.ErrInvalidSwapMode, in.SwapMode)
	}
	// The two standing rules for a loader claim, asked once. It requires an
	// OutboundDestination — without one the post-swap bin has nowhere to go and
	// the node deadlocks — and it must auto-confirm delivery, because the
	// operator's own action at the window IS the acknowledgement.
	//
	// THIS ARM IS NOW UNREACHABLE, and it took a data change to make it so. It
	// used to be live: the claim editor could neither author nor edit a loader
	// claim (the dropdown drops the option, the claims list renders a read-only
	// "Loader" badge in place of Edit), but the COMPARE GRID could — claimToBody
	// echoes swap_mode verbatim and saveCompareCell has an explicit manual_swap
	// branch for the payload cell (www/static/js/pages/processes.js) — so a
	// person clicking a loader's cell saved through here. Closing the allowlist
	// while such a row still existed would have turned that click into an
	// operator-visible error, which is why the rows were quarantined FIRST and
	// the allowlist closed after. Once a loader sync has run the grid has no
	// loader cell to offer, so nothing reaches this arm. Before the first
	// non-empty sync the rows are still stored — and cloneStyleTx copies them into
	// any style cloned meanwhile — so a click there gets the allowlist's refusal
	// instead.
	//
	// It stays as an assert rather than a branch: the invariant is about what a
	// loader claim must look like, and it costs nothing to keep saying so.
	if in.IsLoaderNode() {
		if in.OutboundDestination == "" {
			return 0, fmt.Errorf("manual_swap claims require outbound_destination to be set")
		}
		in.AutoConfirm = true
	}
	// Attribution: an empty Source is an internal caller with nothing to say
	// and reads as admin, the same answer a legacy row gives. Anything else
	// must be one of the four writers.
	if in.Source == "" {
		in.Source = domain.ClaimSourceAdmin
	}
	if !domain.IsClaimSource(in.Source) {
		return 0, fmt.Errorf("claim source must be admin, hmi, generated or cloned, got %q", in.Source)
	}
	// The per-mode arms, one refusal, naming one field — see modeArmViolation.
	if err := modeArmViolation(in); err != nil {
		return 0, err
	}
	// IndexRobotSupplies describes the CELL'S HARDWARE — which robot can reach
	// the supermarket from that press. Two styles on one press disagreeing
	// about it is not a configuration, it is an operator who edited one style
	// and not the other, and the symptom is a press that choreographs
	// differently depending on what it is running.
	//
	// A WARNING, NOT A REFUSAL, and the direction matters: refusing would make
	// the flag unsettable, because changing a press with four styles means
	// four saves and the first three would each be refused by the other three.
	warnIndexRobotSuppliesDrift(db, in)

	// Withheld at the store as well as at ingress, for the same reason the
	// guards above are: a non-API caller must not write what the API refuses.
	// An absent flag leaves a stored one untouched — nothing here migrates data.
	if in.KeepStaged != nil && *in.KeepStaged {
		return 0, fmt.Errorf("keep_staged: %s", domain.KeepStagedWithheld)
	}

	var existingID int64
	err := db.QueryRow(`SELECT id FROM style_node_claims WHERE style_id=? AND core_node_name=?`,
		in.StyleID, in.CoreNodeName).Scan(&existingID)
	if err == nil {
		return existingID, updateClaim(db, existingID, in)
	}

	// INSERT takes the documented defaults for the absent-means-untouched
	// columns: a claim has to have a board position, a provenance and two
	// flag values from the moment it exists. Only UPDATE can leave a column
	// alone, because only UPDATE has a prior value to leave.
	sequence := 0
	if in.Sequence != nil {
		sequence = *in.Sequence
	}
	if sequence <= 0 {
		var maxSeq int
		db.QueryRow(`SELECT COALESCE(MAX(sequence), 0) FROM style_node_claims WHERE style_id=?`, in.StyleID).Scan(&maxSeq)
		sequence = maxSeq + 1
	}
	autoReorder := in.AutoReorder != nil && *in.AutoReorder
	indexRobotSupplies := in.IndexRobotSupplies != nil && *in.IndexRobotSupplies
	keepStaged := in.KeepStaged != nil && *in.KeepStaged
	allowedJSON := marshalAllowedPayloads(in.AllowedPayloadCodes)
	// INSERT OR IGNORE: if a concurrent writer inserted the same
	// (style_id, core_node_name) between our SELECT above and this
	// INSERT, RowsAffected==0 and we fall through to UPDATE the
	// winner's row with our values. Plain INSERT failed here with
	// UNIQUE constraint on the same race.
	source := "legacy"
	if in.ReorderPointSource != nil && *in.ReorderPointSource != "" {
		source = *in.ReorderPointSource
	}
	res, err := db.Exec(`INSERT OR IGNORE INTO style_node_claims (style_id, core_node_name, role, swap_mode, payload_code,
		reorder_point, reorder_point_source, auto_reorder, inbound_staging, outbound_staging,
		inbound_source, outbound_destination, allowed_payload_codes, auto_request_payload,
		keep_staged, evacuate_on_changeover, paired_core_node, auto_confirm, sequence,
		lineside_soft_threshold, second_paired_core_node, reuse_compatible_bins, auto_push,
		changeover_evac_nodes, changeover_evac_destination,
		index_robot_supplies, key_route, key_task, changeover_carryover_disposition,
		source, called_by, updated_at, source_preset_id, source_preset_version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		        ?, ?, datetime('now'), ?, ?)`,
		in.StyleID, in.CoreNodeName, in.Role, in.SwapMode, in.PayloadCode,
		in.ReorderPoint, source, autoReorder, in.InboundStaging, in.OutboundStaging,
		in.InboundSource, in.OutboundDestination, allowedJSON, in.AutoRequestPayload,
		keepStaged, in.EvacuateOnChangeover, in.PairedCoreNode, in.AutoConfirm, sequence,
		in.LinesideSoftThreshold, in.SecondPairedCoreNode, in.ReuseCompatibleBins, in.AutoPush,
		marshalEvacNodes(domain.OptValue(in.ChangeoverEvacNodes)),
		domain.OptValue(in.ChangeoverEvacDestination),
		indexRobotSupplies, marshalKeyRoute(domain.OptValue(in.KeyRoute)),
		domain.OptValue(in.KeyTask), carryoverOrDefault(in.ChangeoverCarryoverDisposition),
		in.Source, in.CalledBy, in.SourcePresetID, in.SourcePresetVersion)
	if err != nil {
		return 0, err
	}
	if affected, _ := res.RowsAffected(); affected == 1 {
		return res.LastInsertId()
	}
	if err := db.QueryRow(`SELECT id FROM style_node_claims WHERE style_id=? AND core_node_name=?`,
		in.StyleID, in.CoreNodeName).Scan(&existingID); err != nil {
		return 0, err
	}
	return existingID, updateClaim(db, existingID, in)
}

// updateClaim writes the columns the caller expressed an opinion about.
//
// The always-written set is every column with exactly one writer. The
// pointer-typed ones below it are the columns a writer can decline to speak
// about (see the contract on NodeClaimInput): this used to be one
// unconditional UPDATE of every column, which meant a caller sending a subset
// silently reset the rest to their zero values on every save.
//
// The set grew from four to nine because the second half of the disease was
// found the same way as the first: five columns added later were put in the
// unconditional list on the argument that the claims editor always fills them
// in. The replenishment admin page is also a writer, and a reorder-point edit
// wiped a press's evacuation positions, its evacuation destination, the loader
// card and the key route.
// modeArmViolation is the per-mode arm of UpsertClaim: the fields a swap mode
// needs present or absent before a row may be written, refused one at a
// time with the first violation named. two_robot and two_robot_press_index
// keep their hand-written arms; the strict modes read flowspec.Steady through
// domain.SteadyViolations. Split out of UpsertClaim when the attribution,
// D2 and D4 guards landed together and pushed it past the funlen ceiling;
// behaviour is unchanged — every refusal here was a refusal there.
func modeArmViolation(in NodeClaimInput) error {
	// two_robot claims require InboundStaging. Robot A drops the new bin
	// at the staging node and waits there with a wait-with-node step until
	// Robot B clears the production node. Without InboundStaging the
	// dispatcher has no hand-off point and BuildTwoRobotSwapSteps returns
	// (nil, nil) silently — the operator's RELEASE click does nothing and
	// the failure mode is invisible. Validating at config time means the
	// runtime no-op at material_orders.go BuildTwoRobotSwapSteps becomes
	// unreachable defensive code (kept as an assert, not a real branch).
	// Phase 2 #9 of 2026-04-27 v2 direction doc.
	if in.SwapMode == protocol.SwapModeTwoRobot && in.InboundStaging == "" {
		return fmt.Errorf("two_robot claims require inbound_staging to be set")
	}
	// ...and OutboundDestination: robot B takes the old bin straight there,
	// the planner refuses a pair without one, and the API refuses the claim
	// (flowspec D2). A store that did not would still let an import write it.
	if in.SwapMode == protocol.SwapModeTwoRobot && in.OutboundDestination == "" {
		return fmt.Errorf("two_robot claims require outbound_destination to be set")
	}
	// two_robot_press_index claims need PairedCoreNode (back position B) and
	// OutboundDestination. R1's multi-step ComplexOrder carries the full bin
	// from A → outbound and the replacement from inbound → B (or C in the
	// 3-position layout); R2 indexes B → A (and C → B in 3-position).
	// Without PairedCoreNode or OutboundDestination, BuildTwoRobotPressIndexSwapSteps
	// returns nil and the operator's RELEASE silently no-ops.
	if in.SwapMode == protocol.SwapModeTwoRobotPressIndex {
		if in.PairedCoreNode == "" {
			return fmt.Errorf("two_robot_press_index claims require paired_core_node (back position) to be set")
		}
		if in.OutboundDestination == "" {
			return fmt.Errorf("two_robot_press_index claims require outbound_destination to be set")
		}
		// Optional 3-position: SecondPairedCoreNode must be distinct from
		// the front and the back to avoid a step with pickup == dropoff.
		if in.SecondPairedCoreNode != "" {
			if in.SecondPairedCoreNode == in.CoreNodeName {
				return fmt.Errorf("second_paired_core_node must differ from core_node_name (front position)")
			}
			if in.SecondPairedCoreNode == in.PairedCoreNode {
				return fmt.Errorf("second_paired_core_node must differ from paired_core_node (back position)")
			}
		}
	}
	// THE STRICT MODES READ ONE TABLE (D4). single_robot had no arm here at
	// all; it now takes its whole row from flowspec.Steady through
	// domain.SteadyViolations, exactly as ValidateNodeClaim reads it — every
	// Required field refused blank, every Forbidden field refused populated —
	// so a non-API writer meets the answer the API gives: an import cannot
	// store a single_robot claim with no staging or no destination that the
	// planner would refuse after START. sequential was meant to join it and is
	// held; see domain.StrictSteadyModes for the measurement.
	// The first violation is the refusal, as with every arm above: one error,
	// naming one field, in the row's canonical order.
	if violations := domain.SteadyViolations(in); len(violations) > 0 {
		v := violations[0]
		if v.Need == flowspec.Required {
			return fmt.Errorf("%s claims require %s to be set", in.SwapMode, v.Field)
		}
		return fmt.Errorf("%s claims do not use %s; clear it", in.SwapMode, v.Field)
	}
	return nil
}

// warnIndexRobotSuppliesDrift logs when this save would leave two styles on the
// same press disagreeing about which robot fetches the replacement.
//
// Scoped to claims on the SAME core node in the same process, because that is
// what "one press" means; two presses may legitimately differ.
//
// Silent on a caller with no opinion (nil) — an import or the compare grid is
// not asserting anything about the hardware and must not be reported as if it
// were.
func warnIndexRobotSuppliesDrift(db DBTX, in NodeClaimInput) {
	if in.IndexRobotSupplies == nil || in.CoreNodeName == "" {
		return
	}
	want := *in.IndexRobotSupplies
	rows, err := db.Query(`
		SELECT s.name, c.index_robot_supplies
		FROM style_node_claims c
		JOIN styles s ON s.id = c.style_id
		WHERE c.core_node_name = ?
		  AND c.style_id != ?
		  AND s.deleted_at IS NULL
		  AND c.retired_at IS NULL
		  AND s.process_id = (SELECT process_id FROM styles WHERE id = ?)`,
		in.CoreNodeName, in.StyleID, in.StyleID)
	if err != nil {
		// Cannot check is not a finding. Saying nothing is right here: the
		// save proceeds either way and a warning we could not substantiate
		// would be worse than none.
		return
	}
	defer rows.Close()
	var disagree []string
	for rows.Next() {
		var styleName string
		var flag bool
		if err := rows.Scan(&styleName, &flag); err != nil {
			return
		}
		if flag != want {
			disagree = append(disagree, styleName)
		}
	}
	if len(disagree) > 0 {
		log.Printf("claim %s: index_robot_supplies=%v disagrees with style(s) %s on the same press. "+
			"This flag describes which robot can reach the supermarket — a fact about the cell, not "+
			"about a style — so the press will choreograph differently depending on what it runs. "+
			"Set it the same on every style for this node.",
			in.CoreNodeName, want, strings.Join(disagree, ", "))
	}
}

func updateClaim(db DBTX, id int64, in NodeClaimInput) error {
	allowedJSON := marshalAllowedPayloads(in.AllowedPayloadCodes)

	// Attribution is always written: the row says who LAST wrote it, and
	// updated_at is when. retired_at is cleared — an upsert onto a retired
	// (style, node) is the same claim coming back, under the same id, so the
	// history that points at it keeps pointing at a real row.
	sets := []string{
		`role=?`, `swap_mode=?`, `payload_code=?`, `reorder_point=?`,
		`inbound_staging=?`, `outbound_staging=?`, `inbound_source=?`, `outbound_destination=?`,
		`allowed_payload_codes=?`, `auto_request_payload=?`, `evacuate_on_changeover=?`,
		`paired_core_node=?`, `auto_confirm=?`, `lineside_soft_threshold=?`,
		`second_paired_core_node=?`, `reuse_compatible_bins=?`, `auto_push=?`,
		`source=?`, `called_by=?`, `updated_at=datetime('now')`, `retired_at=NULL`,
	}
	args := []any{
		in.Role, in.SwapMode, in.PayloadCode, in.ReorderPoint,
		in.InboundStaging, in.OutboundStaging, in.InboundSource, in.OutboundDestination,
		allowedJSON, in.AutoRequestPayload, in.EvacuateOnChangeover,
		in.PairedCoreNode, in.AutoConfirm, in.LinesideSoftThreshold,
		in.SecondPairedCoreNode, in.ReuseCompatibleBins, in.AutoPush,
		in.Source, in.CalledBy,
	}
	if in.SourcePresetID != nil {
		sets, args = append(sets, `source_preset_id=?`), append(args, *in.SourcePresetID)
	}
	if in.SourcePresetVersion != nil {
		sets, args = append(sets, `source_preset_version=?`), append(args, *in.SourcePresetVersion)
	}

	if in.ReorderPointSource != nil {
		// An explicit empty string still means "legacy" — the column has never
		// been allowed to hold "", and a caller that speaks is answered.
		source := *in.ReorderPointSource
		if source == "" {
			source = "legacy"
		}
		sets, args = append(sets, `reorder_point_source=?`), append(args, source)
	}
	if in.AutoReorder != nil {
		sets, args = append(sets, `auto_reorder=?`), append(args, *in.AutoReorder)
	}
	if in.KeepStaged != nil {
		sets, args = append(sets, `keep_staged=?`), append(args, *in.KeepStaged)
	}
	if in.Sequence != nil {
		sets, args = append(sets, `sequence=?`), append(args, *in.Sequence)
	}
	if in.IndexRobotSupplies != nil {
		sets, args = append(sets, `index_robot_supplies=?`), append(args, *in.IndexRobotSupplies)
	}
	if in.ChangeoverEvacNodes != nil {
		sets, args = append(sets, `changeover_evac_nodes=?`), append(args, marshalEvacNodes(*in.ChangeoverEvacNodes))
	}
	if in.ChangeoverEvacDestination != nil {
		sets, args = append(sets, `changeover_evac_destination=?`), append(args, *in.ChangeoverEvacDestination)
	}
	if in.ChangeoverCarryoverDisposition != nil {
		sets, args = append(sets, `changeover_carryover_disposition=?`), append(args, string(*in.ChangeoverCarryoverDisposition))
	}
	if in.KeyRoute != nil {
		sets, args = append(sets, `key_route=?`), append(args, marshalKeyRoute(*in.KeyRoute))
	}
	if in.KeyTask != nil {
		sets, args = append(sets, `key_task=?`), append(args, *in.KeyTask)
	}

	args = append(args, id)
	_, err := db.Exec(`UPDATE style_node_claims SET `+strings.Join(sets, ", ")+` WHERE id=?`, args...)
	return err
}

// marshalEvacNodes stores the per-position tooling-relevance set the same way
// allowed_payload_codes is stored: a JSON array, and the EMPTY STRING for an
// empty set rather than "[]", so "no position marked" reads identically on a row
// written today and a row that predates the column.
// marshalKeyRoute stores the ordered via-point list. ORDER IS MEANINGFUL to
// SEER, so this is a JSON array and not a set — the same encoding as the
// allowed-payload and evac-position lists, for the same reason: one TEXT column,
// no join table, and nothing here is ever queried by element.
func marshalKeyRoute(points []string) string {
	return marshalAllowedPayloads(points)
}

func marshalEvacNodes(positions []string) string {
	return marshalAllowedPayloads(positions)
}

func marshalAllowedPayloads(codes []string) string {
	if len(codes) == 0 {
		return ""
	}
	data, _ := json.Marshal(codes)
	return string(data)
}

// DeleteClaim removes a claim row by id — unless changeover history still
// points at it, in which case the row is RETIRED instead.
//
// changeover_node_tasks.from_claim_id / to_claim_id are how a past
// changeover's "from part X to part Y" label resolves. Edge runs with foreign
// keys OFF, so a hard DELETE here left those pointers dangling and the label
// blank. A retired row keeps the label readable; every list read skips it
// (liveClaims), and re-adding the same (style, node) claim revives it under
// the same id.
func DeleteClaim(db DBTX, id int64) error {
	// THE FLOOR EVERY DOOR STANDS ON. SaveFlow refuses a position move on the
	// running style, but it was the only door that did — the legacy
	// POST/DELETE /api/style-node-claims and the replenishment page's write
	// walked straight past it.
	//
	// GATED ON THE BIN, NOT ON "IS THIS STYLE RUNNING". The hazard is a
	// CARRIER stranded: take PLN_01 off the running flow while a bin stands on
	// it and the level sweep never refills it, the PLC tick stops counting the
	// parts made off it, and every delivered/completed handler looks past it —
	// all of them resolve by (active_style_id, core_node_name). With no bin
	// there is no carrier to strand, and refusing anyway would forbid ordinary
	// setup: creating a process, setting its active style and then editing its
	// positions is what every seeder, the sim and 500-odd tests do.
	//
	// The composer's own guard stays and stays STRICTER — it sees the whole
	// flow, so it can refuse the move before anything is written and say
	// "PLN_01 is running, move it to PLN_03 after the next changeover". This
	// one sees a row, and it is the half that cannot be bypassed.
	if err := refuseStrandingARunningBin(db, id); err != nil {
		return err
	}
	var refs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM changeover_node_tasks
		WHERE from_claim_id = ?1 OR to_claim_id = ?1`, id).Scan(&refs); err != nil {
		return fmt.Errorf("claim %d: count history references: %w", id, err)
	}
	if refs > 0 {
		_, err := db.Exec(`UPDATE style_node_claims
			SET retired_at = datetime('now'), updated_at = datetime('now')
			WHERE id = ? AND retired_at IS NULL`, id)
		return err
	}
	_, err := db.Exec(`DELETE FROM style_node_claims WHERE id=?`, id)
	return err
}

// refuseStrandingARunningBin refuses deleting a claim whose position is (a) on
// the style its process is RUNNING and (b) physically holding a carrier.
//
// One statement on the delete path. The read paths never ask.
func refuseStrandingARunningBin(db DBTX, id int64) error {
	var node string
	err := db.QueryRow(`
		SELECT c.core_node_name
		FROM style_node_claims c
		JOIN processes p ON p.active_style_id = c.style_id
		JOIN process_nodes pn ON pn.process_id = p.id
		     AND pn.core_node_name = c.core_node_name AND pn.deleted_at IS NULL
		JOIN process_node_runtime_states r ON r.process_node_id = pn.id
		WHERE c.id = ? AND c.retired_at IS NULL AND r.active_bin_id IS NOT NULL`, id).Scan(&node)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil // not running, or nothing standing on it
	case err != nil:
		return fmt.Errorf("claim %d: running-bin check: %w", id, err)
	}
	return fmt.Errorf("%w: %s is on the style this press is running and has a bin on it — take it off the flow after the next changeover",
		domain.ErrRunningPositionMove, node)
}

// carryoverOrDefault writes 'replace' when the caller said nothing, matching
// the column default. The zero value of the type is the empty string, and an
// empty string in this column would read as "unset" to anything that checks it
// literally rather than through domain.CarryoverFor.
func carryoverOrDefault(d *domain.CarryoverDisposition) string {
	if d == nil || *d == "" {
		return string(domain.CarryoverReplace)
	}
	return string(*d)
}

// StampClaimPresetProvenance writes source_preset_id and source_preset_version
// on the live claims of one style, and NOTHING else.
//
// A DEDICATED NARROW UPDATE, because updateClaim is not the tool for this.
// updateClaim writes eighteen columns unconditionally (claims.go's own
// boundary note), so putting provenance through it means a caller who wanted
// to record where a flow came from has to first reproduce the flow exactly or
// silently rewrite it. Naming a migration candidate stamps rows an engineer
// has not looked at; the one thing it may not do is move a column.
//
// TWO COLUMNS, AND NOT updated_at. It moved it — the trigger-free convention
// every other write here follows — and that told the floor something untrue.
//
// updated_at has exactly one reader on this path: flowProvenance
// (service/station_composer.go), which turns it into the operator's set-up card
// sentence `Flow saved 09-12 from the desktop`. Naming one migration candidate
// stamps every style that already runs that shape, so on a press with ninety
// parts it would have moved ninety of those dates to today — telling an
// operator that an engineer changed the flow, on a day nobody touched a flow.
// Recording where a shape came from is not a change to the shape.
//
// Between the column's literal meaning ("this row was written") and the
// sentence an operator reads, the sentence wins. If something later needs "when
// was this row last touched at all", that is a different question and deserves
// its own column rather than this one's second meaning.
//
// Live claims only: a retired row's provenance is history.
func StampClaimPresetProvenance(db *sql.DB, styleID, presetID int64, version int) (int64, error) {
	res, err := db.Exec(`UPDATE style_node_claims
		SET source_preset_id = ?, source_preset_version = ?
		WHERE style_id = ? AND retired_at IS NULL`, presetID, version, styleID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
