package store

import (
	"encoding/json"
	"fmt"
	"time"
)

// StyleNodeClaim declares that a style needs a specific core node with a given
// payload and role. Three roles are supported:
//   - "consume": system delivers full bins and removes empties
//   - "produce": system delivers empty bins and removes filled ones
//   - "changeover": temporary role during style transitions (evacuate and restore material)
//
// SwapMode controls the choreography:
//   - "simple": PLC-driven reorder at threshold
//   - "sequential": backfill while current bin is in transit
//   - "single_robot": inbound + outbound staging for single-robot swap
//   - "two_robot": dual-robot swap with inbound staging
//   - "manual_swap": operator-driven forklift swap with multi-order queue
//
// Routing fields follow a directional convention:
//
//	InboundSource → InboundStaging → CoreNodeName → OutboundStaging → OutboundDestination
//
// InboundSource is where inbound material is picked up FROM.
// OutboundDestination is where outbound material is dropped off TO.
type StyleNodeClaim struct {
	ID                       int64     `json:"id"`
	StyleID                  int64     `json:"style_id"`
	CoreNodeName             string    `json:"core_node_name"`
	Role                     string    `json:"role"`      // "consume", "produce", or "changeover"
	SwapMode                 string    `json:"swap_mode"`  // "simple", "single_robot", "two_robot", "sequential", "manual_swap"
	PayloadCode              string    `json:"payload_code"`
	UOPCapacity              int       `json:"uop_capacity"`
	ReorderPoint             int       `json:"reorder_point"`
	AutoReorder              bool      `json:"auto_reorder"`
	InboundStaging           string    `json:"inbound_staging"`
	OutboundStaging          string    `json:"outbound_staging"`
	InboundSource            string    `json:"inbound_source"`
	OutboundDestination           string    `json:"outbound_destination"`
	AllowedPayloadCodes      []string  `json:"allowed_payload_codes"`
	AutoRequestPayload       string    `json:"auto_request_payload"`
	KeepStaged               bool      `json:"keep_staged"`
	EvacuateOnChangeover     bool      `json:"evacuate_on_changeover"`
	PairedCoreNode           string    `json:"paired_core_node"` // A/B cycling: names the alternate node
	AutoConfirm              bool      `json:"auto_confirm"`     // manual_swap: auto-confirm delivery without operator acknowledgement
	Sequence                 int       `json:"sequence"`
	CreatedAt                time.Time `json:"created_at"`
}

// AllowedPayloads returns the effective set of payload codes this claim accepts.
// For source nodes with an allowed list, returns that list. Otherwise returns
// a single-element list with the primary payload code.
func (c *StyleNodeClaim) AllowedPayloads() []string {
	if len(c.AllowedPayloadCodes) > 0 {
		return c.AllowedPayloadCodes
	}
	if c.PayloadCode != "" {
		return []string{c.PayloadCode}
	}
	return nil
}

type StyleNodeClaimInput struct {
	StyleID                 int64  `json:"style_id"`
	CoreNodeName            string `json:"core_node_name"`
	Role                    string `json:"role"`
	SwapMode                string `json:"swap_mode"`
	PayloadCode             string `json:"payload_code"`
	UOPCapacity             int    `json:"uop_capacity"`
	ReorderPoint            int    `json:"reorder_point"`
	AutoReorder             bool   `json:"auto_reorder"`
	InboundStaging          string `json:"inbound_staging"`
	OutboundStaging         string `json:"outbound_staging"`
	InboundSource           string `json:"inbound_source"`
	OutboundDestination          string `json:"outbound_destination"`
	AllowedPayloadCodes     []string `json:"allowed_payload_codes"`
	AutoRequestPayload      string   `json:"auto_request_payload"`
	KeepStaged              bool     `json:"keep_staged"`
	EvacuateOnChangeover    bool     `json:"evacuate_on_changeover"`
	PairedCoreNode          string   `json:"paired_core_node"`
	AutoConfirm             bool     `json:"auto_confirm"`
	Sequence                int      `json:"sequence"`
}

const claimSelect = `id, style_id, core_node_name, role, swap_mode, payload_code,
	uop_capacity, reorder_point, auto_reorder, inbound_staging, outbound_staging,
	inbound_source, outbound_destination, allowed_payload_codes, auto_request_payload,
	keep_staged, evacuate_on_changeover, paired_core_node, auto_confirm, sequence, created_at`

func scanStyleNodeClaim(scanner interface{ Scan(...interface{}) error }) (StyleNodeClaim, error) {
	var c StyleNodeClaim
	var createdAt, allowedJSON string
	if err := scanner.Scan(&c.ID, &c.StyleID, &c.CoreNodeName, &c.Role, &c.SwapMode, &c.PayloadCode,
		&c.UOPCapacity, &c.ReorderPoint, &c.AutoReorder, &c.InboundStaging, &c.OutboundStaging,
		&c.InboundSource, &c.OutboundDestination, &allowedJSON, &c.AutoRequestPayload,
		&c.KeepStaged, &c.EvacuateOnChangeover, &c.PairedCoreNode, &c.AutoConfirm, &c.Sequence, &createdAt); err != nil {
		return c, err
	}
	c.CreatedAt = scanTime(createdAt)
	if allowedJSON != "" {
		_ = json.Unmarshal([]byte(allowedJSON), &c.AllowedPayloadCodes)
	}
	return c, nil
}

func (db *DB) ListStyleNodeClaims(styleID int64) ([]StyleNodeClaim, error) {
	rows, err := db.Query(`SELECT `+claimSelect+`
		FROM style_node_claims WHERE style_id=? ORDER BY sequence, core_node_name`, styleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StyleNodeClaim
	for rows.Next() {
		c, err := scanStyleNodeClaim(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (db *DB) GetStyleNodeClaim(id int64) (*StyleNodeClaim, error) {
	c, err := scanStyleNodeClaim(db.QueryRow(`SELECT `+claimSelect+`
		FROM style_node_claims WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (db *DB) GetStyleNodeClaimByNode(styleID int64, coreNodeName string) (*StyleNodeClaim, error) {
	c, err := scanStyleNodeClaim(db.QueryRow(`SELECT `+claimSelect+`
		FROM style_node_claims WHERE style_id=? AND core_node_name=?`, styleID, coreNodeName))
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (db *DB) UpsertStyleNodeClaim(in StyleNodeClaimInput) (int64, error) {
	if in.Role != "produce" && in.Role != "changeover" {
		in.Role = "consume"
	}
	if in.SwapMode == "" {
		in.SwapMode = "simple"
	}
	// manual_swap claims require OutboundDestination — without it the post-swap
	// bin has nowhere to go and the node deadlocks.
	if in.SwapMode == "manual_swap" && in.OutboundDestination == "" {
		return 0, fmt.Errorf("manual_swap claims require outbound_destination to be set")
	}
	// manual_swap claims must auto-confirm delivery (operator action IS the acknowledgement).
	if in.SwapMode == "manual_swap" {
		in.AutoConfirm = true
	}
	var existingID int64
	err := db.QueryRow(`SELECT id FROM style_node_claims WHERE style_id=? AND core_node_name=?`,
		in.StyleID, in.CoreNodeName).Scan(&existingID)
	if err == nil {
		allowedJSON := marshalAllowedPayloads(in.AllowedPayloadCodes)
		_, err = db.Exec(`UPDATE style_node_claims SET role=?, swap_mode=?, payload_code=?,
			uop_capacity=?, reorder_point=?, auto_reorder=?, inbound_staging=?, outbound_staging=?,
			inbound_source=?, outbound_destination=?, allowed_payload_codes=?, auto_request_payload=?,
			keep_staged=?, evacuate_on_changeover=?, paired_core_node=?, auto_confirm=?, sequence=?
			WHERE id=?`,
			in.Role, in.SwapMode, in.PayloadCode, in.UOPCapacity, in.ReorderPoint, in.AutoReorder,
			in.InboundStaging, in.OutboundStaging,
			in.InboundSource, in.OutboundDestination, allowedJSON, in.AutoRequestPayload,
			in.KeepStaged, in.EvacuateOnChangeover, in.PairedCoreNode, in.AutoConfirm, in.Sequence, existingID)
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
		keep_staged, evacuate_on_changeover, paired_core_node, auto_confirm, sequence)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.StyleID, in.CoreNodeName, in.Role, in.SwapMode, in.PayloadCode,
		in.UOPCapacity, in.ReorderPoint, in.AutoReorder, in.InboundStaging, in.OutboundStaging,
		in.InboundSource, in.OutboundDestination, allowedJSON, in.AutoRequestPayload,
		in.KeepStaged, in.EvacuateOnChangeover, in.PairedCoreNode, in.AutoConfirm, in.Sequence)
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

func (db *DB) DeleteStyleNodeClaim(id int64) error {
	_, err := db.Exec(`DELETE FROM style_node_claims WHERE id=?`, id)
	return err
}
