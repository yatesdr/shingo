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
	"fmt"
	"log"
	"slices"
	"strings"

	"shingo/protocol"
	"shingoedge/domain"
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

const claimSelect = `id, style_id, core_node_name, role, swap_mode, payload_code,
	uop_capacity, reorder_point, reorder_point_source, auto_reorder, inbound_staging, outbound_staging,
	inbound_source, outbound_destination, allowed_payload_codes, auto_request_payload,
	keep_staged, evacuate_on_changeover, paired_core_node, auto_confirm, sequence,
	lineside_soft_threshold, second_paired_core_node,
	reuse_compatible_bins, auto_push, below_reorder_since, created_at,
	changeover_evac_positions, changeover_evac_destination, changeover_load_directive,
	index_robot_supplies, key_route, key_task, changeover_carryover_disposition`

func scanNodeClaim(scanner interface{ Scan(...any) error }) (NodeClaim, error) {
	var c NodeClaim
	var createdAt, allowedJSON, evacPositionsJSON, keyRouteJSON string
	var belowSince sql.NullString
	if err := scanner.Scan(&c.ID, &c.StyleID, &c.CoreNodeName, &c.Role, &c.SwapMode, &c.PayloadCode,
		&c.UOPCapacity, &c.ReorderPoint, &c.ReorderPointSource, &c.AutoReorder, &c.InboundStaging, &c.OutboundStaging,
		&c.InboundSource, &c.OutboundDestination, &allowedJSON, &c.AutoRequestPayload,
		&c.KeepStaged, &c.EvacuateOnChangeover, &c.PairedCoreNode, &c.AutoConfirm, &c.Sequence,
		&c.LinesideSoftThreshold, &c.SecondPairedCoreNode,
		&c.ReuseCompatibleBins, &c.AutoPush, &belowSince, &createdAt,
		&evacPositionsJSON, &c.ChangeoverEvacDestination, &c.ChangeoverLoadDirective,
		&c.IndexRobotSupplies, &keyRouteJSON, &c.KeyTask, &c.ChangeoverCarryoverDisposition); err != nil {
		return c, err
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
	if evacPositionsJSON != "" {
		_ = json.Unmarshal([]byte(evacPositionsJSON), &c.ChangeoverEvacPositions)
	}
	if keyRouteJSON != "" {
		_ = json.Unmarshal([]byte(keyRouteJSON), &c.KeyRoute)
	}
	return c, nil
}

// ListClaims returns every claim for a style.
func ListClaims(db *sql.DB, styleID int64) ([]NodeClaim, error) {
	rows, err := db.Query(`SELECT `+claimSelect+`
		FROM style_node_claims WHERE style_id=? ORDER BY sequence, core_node_name`, styleID)
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

// GetClaimByNode returns a claim by its (style_id, core_node_name) pair.
func GetClaimByNode(db *sql.DB, styleID int64, coreNodeName string) (*NodeClaim, error) {
	c, err := scanNodeClaim(db.QueryRow(`SELECT `+claimSelect+`
		FROM style_node_claims WHERE style_id=? AND core_node_name=?`, styleID, coreNodeName))
	if err != nil {
		return nil, err
	}
	return &c, nil
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
func UpsertClaim(db *sql.DB, in NodeClaimInput) (int64, error) {
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
	// mode. The editor defaults new claims to single_robot, so the normal path
	// never hits this; a blank here is a non-UI caller (import, stale API poke).
	// No mode is a safe default: two_robot needs inbound staging and
	// single_robot needs inbound+outbound staging, so any default would only
	// trade a mode error for a more misleading staging error.
	if in.SwapMode == "" {
		return 0, fmt.Errorf("%w: swap_mode is required", protocol.ErrInvalidSwapMode)
	}
	// SwapMode allowlist, keyed on protocol.ConfigurableSwapModes() so it can
	// never drift from the editor dropdown or its drift test. The retired
	// "simple" is deliberately absent — it survives only as a runtime CycleMode
	// descriptor, never a persisted claim mode. "press_position" (the
	// per-position fan-out marker) is in-memory only and must never persist; the
	// allowlist also rejects typos and stale import values.
	if !slices.Contains(protocol.ConfigurableSwapModes(), in.SwapMode) {
		return 0, fmt.Errorf("%w: %q is not a configurable swap_mode", protocol.ErrInvalidSwapMode, in.SwapMode)
	}
	// manual_swap claims require OutboundDestination — without it the
	// post-swap bin has nowhere to go and the node deadlocks.
	if in.SwapMode == protocol.SwapModeManualSwap && in.OutboundDestination == "" {
		return 0, fmt.Errorf("manual_swap claims require outbound_destination to be set")
	}
	// manual_swap claims must auto-confirm delivery (operator action IS
	// the acknowledgement).
	if in.SwapMode == protocol.SwapModeManualSwap {
		in.AutoConfirm = true
	}
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
		return 0, fmt.Errorf("two_robot claims require inbound_staging to be set")
	}
	// two_robot_press_index claims need PairedCoreNode (back position B) and
	// OutboundDestination. R1's multi-step ComplexOrder carries the full bin
	// from A → outbound and the replacement from inbound → B (or C in the
	// 3-position layout); R2 indexes B → A (and C → B in 3-position).
	// Without PairedCoreNode or OutboundDestination, BuildTwoRobotPressIndexSwapSteps
	// returns nil and the operator's RELEASE silently no-ops.
	if in.SwapMode == protocol.SwapModeTwoRobotPressIndex {
		if in.PairedCoreNode == "" {
			return 0, fmt.Errorf("two_robot_press_index claims require paired_core_node (back position) to be set")
		}
		if in.OutboundDestination == "" {
			return 0, fmt.Errorf("two_robot_press_index claims require outbound_destination to be set")
		}
		// Optional 3-position: SecondPairedCoreNode must be distinct from
		// the front and the back to avoid a step with pickup == dropoff.
		if in.SecondPairedCoreNode != "" {
			if in.SecondPairedCoreNode == in.CoreNodeName {
				return 0, fmt.Errorf("second_paired_core_node must differ from core_node_name (front position)")
			}
			if in.SecondPairedCoreNode == in.PairedCoreNode {
				return 0, fmt.Errorf("second_paired_core_node must differ from paired_core_node (back position)")
			}
		}
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
	loadDirective := in.ChangeoverLoadDirective != nil && *in.ChangeoverLoadDirective
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
		uop_capacity, reorder_point, reorder_point_source, auto_reorder, inbound_staging, outbound_staging,
		inbound_source, outbound_destination, allowed_payload_codes, auto_request_payload,
		keep_staged, evacuate_on_changeover, paired_core_node, auto_confirm, sequence,
		lineside_soft_threshold, second_paired_core_node, reuse_compatible_bins, auto_push,
		changeover_evac_positions, changeover_evac_destination, changeover_load_directive,
		index_robot_supplies, key_route, key_task, changeover_carryover_disposition)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.StyleID, in.CoreNodeName, in.Role, in.SwapMode, in.PayloadCode,
		in.UOPCapacity, in.ReorderPoint, source, autoReorder, in.InboundStaging, in.OutboundStaging,
		in.InboundSource, in.OutboundDestination, allowedJSON, in.AutoRequestPayload,
		keepStaged, in.EvacuateOnChangeover, in.PairedCoreNode, in.AutoConfirm, sequence,
		in.LinesideSoftThreshold, in.SecondPairedCoreNode, in.ReuseCompatibleBins, in.AutoPush,
		marshalEvacPositions(domain.OptValue(in.ChangeoverEvacPositions)),
		domain.OptValue(in.ChangeoverEvacDestination), loadDirective,
		indexRobotSupplies, marshalKeyRoute(domain.OptValue(in.KeyRoute)),
		domain.OptValue(in.KeyTask), carryoverOrDefault(in.ChangeoverCarryoverDisposition))
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
// warnIndexRobotSuppliesDrift logs when this save would leave two styles on the
// same press disagreeing about which robot fetches the replacement.
//
// Scoped to claims on the SAME core node in the same process, because that is
// what "one press" means; two presses may legitimately differ.
//
// Silent on a caller with no opinion (nil) — an import or the compare grid is
// not asserting anything about the hardware and must not be reported as if it
// were.
func warnIndexRobotSuppliesDrift(db *sql.DB, in NodeClaimInput) {
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

func updateClaim(db *sql.DB, id int64, in NodeClaimInput) error {
	allowedJSON := marshalAllowedPayloads(in.AllowedPayloadCodes)

	sets := []string{
		`role=?`, `swap_mode=?`, `payload_code=?`, `uop_capacity=?`, `reorder_point=?`,
		`inbound_staging=?`, `outbound_staging=?`, `inbound_source=?`, `outbound_destination=?`,
		`allowed_payload_codes=?`, `auto_request_payload=?`, `evacuate_on_changeover=?`,
		`paired_core_node=?`, `auto_confirm=?`, `lineside_soft_threshold=?`,
		`second_paired_core_node=?`, `reuse_compatible_bins=?`, `auto_push=?`,
	}
	args := []any{
		in.Role, in.SwapMode, in.PayloadCode, in.UOPCapacity, in.ReorderPoint,
		in.InboundStaging, in.OutboundStaging, in.InboundSource, in.OutboundDestination,
		allowedJSON, in.AutoRequestPayload, in.EvacuateOnChangeover,
		in.PairedCoreNode, in.AutoConfirm, in.LinesideSoftThreshold,
		in.SecondPairedCoreNode, in.ReuseCompatibleBins, in.AutoPush,
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
	if in.ChangeoverEvacPositions != nil {
		sets, args = append(sets, `changeover_evac_positions=?`), append(args, marshalEvacPositions(*in.ChangeoverEvacPositions))
	}
	if in.ChangeoverEvacDestination != nil {
		sets, args = append(sets, `changeover_evac_destination=?`), append(args, *in.ChangeoverEvacDestination)
	}
	if in.ChangeoverCarryoverDisposition != nil {
		sets, args = append(sets, `changeover_carryover_disposition=?`), append(args, string(*in.ChangeoverCarryoverDisposition))
	}
	if in.ChangeoverLoadDirective != nil {
		sets, args = append(sets, `changeover_load_directive=?`), append(args, *in.ChangeoverLoadDirective)
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

// marshalEvacPositions stores the per-position tooling-relevance set the same way
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

func marshalEvacPositions(positions []string) string {
	return marshalAllowedPayloads(positions)
}

func marshalAllowedPayloads(codes []string) string {
	if len(codes) == 0 {
		return ""
	}
	data, _ := json.Marshal(codes)
	return string(data)
}

// DeleteClaim removes a claim row by id.
func DeleteClaim(db *sql.DB, id int64) error {
	_, err := db.Exec(`DELETE FROM style_node_claims WHERE id=?`, id)
	return err
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
