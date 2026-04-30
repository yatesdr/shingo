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

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store/internal/helpers"
)

// NodeClaim and NodeClaimInput are the claim-aggregate data types.
//
// NodeClaim declares that a style needs a specific core node with a
// given payload and role. Three roles are supported:
//   - "consume":   system delivers full bins and removes empties
//   - "produce":   system delivers empty bins and removes filled ones
//   - "changeover": temporary role during style transitions
//
// SwapMode controls the choreography:
//   - "simple":      PLC-driven reorder at threshold
//   - "sequential":  backfill while current bin is in transit
//   - "single_robot": inbound + outbound staging for single-robot swap
//   - "two_robot":   dual-robot swap with inbound staging
//   - "two_robot_press_index": dual-robot press-index swap (R1 carries full
//                    out + replacement in; R2 indexes B→A)
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
	uop_capacity, reorder_point, auto_reorder, inbound_staging, outbound_staging,
	inbound_source, outbound_destination, allowed_payload_codes, auto_request_payload,
	keep_staged, evacuate_on_changeover, paired_core_node, auto_confirm, sequence,
	lineside_soft_threshold, created_at`

func scanNodeClaim(scanner interface{ Scan(...interface{}) error }) (NodeClaim, error) {
	var c NodeClaim
	var createdAt, allowedJSON string
	if err := scanner.Scan(&c.ID, &c.StyleID, &c.CoreNodeName, &c.Role, &c.SwapMode, &c.PayloadCode,
		&c.UOPCapacity, &c.ReorderPoint, &c.AutoReorder, &c.InboundStaging, &c.OutboundStaging,
		&c.InboundSource, &c.OutboundDestination, &allowedJSON, &c.AutoRequestPayload,
		&c.KeepStaged, &c.EvacuateOnChangeover, &c.PairedCoreNode, &c.AutoConfirm, &c.Sequence,
		&c.LinesideSoftThreshold, &createdAt); err != nil {
		return c, err
	}
	c.CreatedAt = helpers.ScanTime(createdAt)
	if allowedJSON != "" {
		_ = json.Unmarshal([]byte(allowedJSON), &c.AllowedPayloadCodes)
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

// UpsertClaim inserts or updates a claim and returns the row id. Validates
// role/swap_mode invariants (manual_swap claims must auto-confirm and
// must declare an outbound destination).
func UpsertClaim(db *sql.DB, in NodeClaimInput) (int64, error) {
	if in.Role != protocol.ClaimRoleProduce && in.Role != protocol.ClaimRoleChangeover {
		in.Role = protocol.ClaimRoleConsume
	}
	if in.SwapMode == "" {
		in.SwapMode = "simple"
	}
	// manual_swap claims require OutboundDestination — without it the
	// post-swap bin has nowhere to go and the node deadlocks.
	if in.SwapMode == "manual_swap" && in.OutboundDestination == "" {
		return 0, fmt.Errorf("manual_swap claims require outbound_destination to be set")
	}
	// manual_swap claims must auto-confirm delivery (operator action IS
	// the acknowledgement).
	if in.SwapMode == "manual_swap" {
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
	if in.SwapMode == "two_robot" && in.InboundStaging == "" {
		return 0, fmt.Errorf("two_robot claims require inbound_staging to be set")
	}
	// two_robot_press_index claims need PairedCoreNode (back position B) and
	// OutboundDestination. R1's multi-step ComplexOrder carries the full bin
	// from A → outbound and the replacement from inbound → B; R2 indexes
	// B → A. Without either field BuildTwoRobotPressIndexSwapSteps returns
	// nil and the operator's RELEASE silently no-ops.
	if in.SwapMode == "two_robot_press_index" {
		if in.PairedCoreNode == "" {
			return 0, fmt.Errorf("two_robot_press_index claims require paired_core_node (back position) to be set")
		}
		if in.OutboundDestination == "" {
			return 0, fmt.Errorf("two_robot_press_index claims require outbound_destination to be set")
		}
	}
	var existingID int64
	err := db.QueryRow(`SELECT id FROM style_node_claims WHERE style_id=? AND core_node_name=?`,
		in.StyleID, in.CoreNodeName).Scan(&existingID)
	if err == nil {
		allowedJSON := marshalAllowedPayloads(in.AllowedPayloadCodes)
		_, err = db.Exec(`UPDATE style_node_claims SET role=?, swap_mode=?, payload_code=?,
			uop_capacity=?, reorder_point=?, auto_reorder=?, inbound_staging=?, outbound_staging=?,
			inbound_source=?, outbound_destination=?, allowed_payload_codes=?, auto_request_payload=?,
			keep_staged=?, evacuate_on_changeover=?, paired_core_node=?, auto_confirm=?, sequence=?,
			lineside_soft_threshold=?
			WHERE id=?`,
			in.Role, in.SwapMode, in.PayloadCode, in.UOPCapacity, in.ReorderPoint, in.AutoReorder,
			in.InboundStaging, in.OutboundStaging,
			in.InboundSource, in.OutboundDestination, allowedJSON, in.AutoRequestPayload,
			in.KeepStaged, in.EvacuateOnChangeover, in.PairedCoreNode, in.AutoConfirm, in.Sequence,
			in.LinesideSoftThreshold, existingID)
		return existingID, err
	}
	if in.Sequence <= 0 {
		var maxSeq int
		db.QueryRow(`SELECT COALESCE(MAX(sequence), 0) FROM style_node_claims WHERE style_id=?`, in.StyleID).Scan(&maxSeq)
		in.Sequence = maxSeq + 1
	}
	allowedJSON := marshalAllowedPayloads(in.AllowedPayloadCodes)
	res, err := db.Exec(`INSERT INTO style_node_claims (style_id, core_node_name, role, swap_mode, payload_code,
		uop_capacity, reorder_point, auto_reorder, inbound_staging, outbound_staging,
		inbound_source, outbound_destination, allowed_payload_codes, auto_request_payload,
		keep_staged, evacuate_on_changeover, paired_core_node, auto_confirm, sequence,
		lineside_soft_threshold)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.StyleID, in.CoreNodeName, in.Role, in.SwapMode, in.PayloadCode,
		in.UOPCapacity, in.ReorderPoint, in.AutoReorder, in.InboundStaging, in.OutboundStaging,
		in.InboundSource, in.OutboundDestination, allowedJSON, in.AutoRequestPayload,
		in.KeepStaged, in.EvacuateOnChangeover, in.PairedCoreNode, in.AutoConfirm, in.Sequence,
		in.LinesideSoftThreshold)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
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
