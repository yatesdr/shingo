package store

import (
	"encoding/json"
	"time"
)

// StyleNodeClaim declares that a style needs a specific core node with a given
// payload and role. For "consume" nodes the system delivers full bins; for
// "produce" nodes the system delivers empty bins.
type StyleNodeClaim struct {
	ID                       int64     `json:"id"`
	StyleID                  int64     `json:"style_id"`
	CoreNodeName             string    `json:"core_node_name"`
	Role                     string    `json:"role"`     // "consume" or "produce"
	SwapMode                 string    `json:"swap_mode"` // "simple", "single_robot", "two_robot", "sequential"
	PayloadCode              string    `json:"payload_code"`
	UOPCapacity              int       `json:"uop_capacity"`
	ReorderPoint             int       `json:"reorder_point"`
	AutoReorder              bool      `json:"auto_reorder"`
	InboundStaging           string    `json:"inbound_staging"`
	OutboundStaging          string    `json:"outbound_staging"`
	InboundSource            string    `json:"inbound_source"`
	OutboundSource           string    `json:"outbound_source"`
	AllowedPayloadCodes      []string  `json:"allowed_payload_codes"`
	KeepStaged               bool      `json:"keep_staged"`
	EvacuateOnChangeover     bool      `json:"evacuate_on_changeover"`
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
	OutboundSource          string `json:"outbound_source"`
	AllowedPayloadCodes     []string `json:"allowed_payload_codes"`
	KeepStaged              bool     `json:"keep_staged"`
	EvacuateOnChangeover    bool     `json:"evacuate_on_changeover"`
	Sequence                int      `json:"sequence"`
}

const claimSelect = `id, style_id, core_node_name, role, swap_mode, payload_code,
	uop_capacity, reorder_point, auto_reorder, inbound_staging, outbound_staging,
	inbound_source, outbound_source, allowed_payload_codes,
	keep_staged, evacuate_on_changeover, sequence, created_at`

func scanStyleNodeClaim(scanner interface{ Scan(...interface{}) error }) (StyleNodeClaim, error) {
	var c StyleNodeClaim
	var createdAt, allowedJSON string
	if err := scanner.Scan(&c.ID, &c.StyleID, &c.CoreNodeName, &c.Role, &c.SwapMode, &c.PayloadCode,
		&c.UOPCapacity, &c.ReorderPoint, &c.AutoReorder, &c.InboundStaging, &c.OutboundStaging,
		&c.InboundSource, &c.OutboundSource, &allowedJSON,
		&c.KeepStaged, &c.EvacuateOnChangeover, &c.Sequence, &createdAt); err != nil {
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
	if in.Role != "produce" && in.Role != "changeover" && in.Role != "bin_loader" {
		in.Role = "consume"
	}
	if in.SwapMode == "" {
		in.SwapMode = "simple"
	}
	var existingID int64
	err := db.QueryRow(`SELECT id FROM style_node_claims WHERE style_id=? AND core_node_name=?`,
		in.StyleID, in.CoreNodeName).Scan(&existingID)
	if err == nil {
		allowedJSON := marshalAllowedPayloads(in.AllowedPayloadCodes)
		_, err = db.Exec(`UPDATE style_node_claims SET role=?, swap_mode=?, payload_code=?,
			uop_capacity=?, reorder_point=?, auto_reorder=?, inbound_staging=?, outbound_staging=?,
			inbound_source=?, outbound_source=?, allowed_payload_codes=?,
			keep_staged=?, evacuate_on_changeover=?, sequence=?
			WHERE id=?`,
			in.Role, in.SwapMode, in.PayloadCode, in.UOPCapacity, in.ReorderPoint, in.AutoReorder,
			in.InboundStaging, in.OutboundStaging,
			in.InboundSource, in.OutboundSource, allowedJSON,
			in.KeepStaged, in.EvacuateOnChangeover, in.Sequence, existingID)
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
		inbound_source, outbound_source, allowed_payload_codes,
		keep_staged, evacuate_on_changeover, sequence)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.StyleID, in.CoreNodeName, in.Role, in.SwapMode, in.PayloadCode,
		in.UOPCapacity, in.ReorderPoint, in.AutoReorder, in.InboundStaging, in.OutboundStaging,
		in.InboundSource, in.OutboundSource, allowedJSON,
		in.KeepStaged, in.EvacuateOnChangeover, in.Sequence)
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
