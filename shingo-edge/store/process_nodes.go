package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode"
)

type ProcessNode struct {
	ID                    int64     `json:"id"`
	ProcessID             int64     `json:"process_id"`
	Code                  string    `json:"code"`
	CoreNodeName          string    `json:"core_node_name"`
	Name                  string    `json:"name"`
	PositionType          string    `json:"position_type"`
	Sequence              int       `json:"sequence"`
	DeliveryNode          string    `json:"delivery_node"`
	StagingNode           string    `json:"staging_node"`
	SecondaryStagingNode  string    `json:"secondary_staging_node"`
	StagingNodeGroup      string    `json:"staging_node_group"`
	SecondaryNodeGroup    string    `json:"secondary_node_group"`
	FullPickupNode        string    `json:"full_pickup_node"`
	FullPickupNodeGroup   string    `json:"full_pickup_node_group"`
	OutgoingNode          string    `json:"outgoing_node"`
	OutgoingNodeGroup     string    `json:"outgoing_node_group"`
	AllowsReorder         bool      `json:"allows_reorder"`
	AllowsEmptyRelease    bool      `json:"allows_empty_release"`
	AllowsPartialRelease  bool      `json:"allows_partial_release"`
	AllowsManifestConfirm bool      `json:"allows_manifest_confirm"`
	AllowsStationChange   bool      `json:"allows_station_change"`
	Enabled               bool      `json:"enabled"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
	DelegatedStationID    *int64    `json:"delegated_station_id,omitempty"`
	DelegatedStationName  string    `json:"delegated_station_name"`
	ProcessName           string    `json:"process_name"`
}

type ProcessNodeInput struct {
	ProcessID             int64  `json:"process_id"`
	DelegatedStationID    *int64 `json:"delegated_station_id,omitempty"`
	Code                  string `json:"code"`
	CoreNodeName          string `json:"core_node_name"`
	Name                  string `json:"name"`
	PositionType          string `json:"position_type"`
	Sequence              int    `json:"sequence"`
	DeliveryNode          string `json:"delivery_node"`
	StagingNode           string `json:"staging_node"`
	SecondaryStagingNode  string `json:"secondary_staging_node"`
	StagingNodeGroup      string `json:"staging_node_group"`
	SecondaryNodeGroup    string `json:"secondary_node_group"`
	FullPickupNode        string `json:"full_pickup_node"`
	FullPickupNodeGroup   string `json:"full_pickup_node_group"`
	OutgoingNode          string `json:"outgoing_node"`
	OutgoingNodeGroup     string `json:"outgoing_node_group"`
	AllowsReorder         bool   `json:"allows_reorder"`
	AllowsEmptyRelease    bool   `json:"allows_empty_release"`
	AllowsPartialRelease  bool   `json:"allows_partial_release"`
	AllowsManifestConfirm bool   `json:"allows_manifest_confirm"`
	AllowsStationChange   bool   `json:"allows_station_change"`
	Enabled               bool   `json:"enabled"`
}

type StationProcessNodeDelegation struct {
	OperatorStationID int64  `json:"operator_station_id"`
	ProcessNodeID     int64  `json:"process_node_id"`
	StationName       string `json:"station_name"`
	NodeName          string `json:"node_name"`
}

const processNodeSelect = `n.id, n.process_id, n.code, n.core_node_name, n.name, n.position_type, n.sequence,
	n.delivery_node, n.staging_node, n.secondary_staging_node,
	n.staging_node_group, n.secondary_node_group, n.full_pickup_node,
	n.full_pickup_node_group, n.outgoing_node, n.outgoing_node_group,
	n.allows_reorder, n.allows_empty_release, n.allows_partial_release,
	n.allows_manifest_confirm, n.allows_station_change, n.enabled,
	n.created_at, n.updated_at, d.operator_station_id, COALESCE(s.name, ''), COALESCE(p.name, '')`

const processNodeJoin = `FROM process_nodes n
	LEFT JOIN operator_station_process_nodes d ON d.process_node_id = n.id
	LEFT JOIN operator_stations s ON s.id = d.operator_station_id
	LEFT JOIN processes p ON p.id = n.process_id`

func scanProcessNode(scanner interface{ Scan(...interface{}) error }) (ProcessNode, error) {
	var n ProcessNode
	var createdAt, updatedAt string
	var delegatedStationID sql.NullInt64
	err := scanner.Scan(
		&n.ID, &n.ProcessID, &n.Code, &n.CoreNodeName, &n.Name, &n.PositionType, &n.Sequence,
		&n.DeliveryNode, &n.StagingNode, &n.SecondaryStagingNode,
		&n.StagingNodeGroup, &n.SecondaryNodeGroup, &n.FullPickupNode,
		&n.FullPickupNodeGroup, &n.OutgoingNode, &n.OutgoingNodeGroup,
		&n.AllowsReorder, &n.AllowsEmptyRelease, &n.AllowsPartialRelease,
		&n.AllowsManifestConfirm, &n.AllowsStationChange, &n.Enabled,
		&createdAt, &updatedAt, &delegatedStationID, &n.DelegatedStationName, &n.ProcessName,
	)
	if err != nil {
		return n, err
	}
	n.CreatedAt = scanTime(createdAt)
	n.UpdatedAt = scanTime(updatedAt)
	if delegatedStationID.Valid {
		id := delegatedStationID.Int64
		n.DelegatedStationID = &id
	}
	return n, nil
}

func scanProcessNodes(rows rowScanner) ([]ProcessNode, error) {
	var out []ProcessNode
	for rows.Next() {
		n, err := scanProcessNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (db *DB) ListProcessNodes() ([]ProcessNode, error) {
	rows, err := db.Query(`SELECT ` + processNodeSelect + ` ` + processNodeJoin + ` ORDER BY n.process_id, n.sequence, n.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanProcessNodes(rows)
}

func (db *DB) ListProcessNodesByProcess(processID int64) ([]ProcessNode, error) {
	rows, err := db.Query(`SELECT `+processNodeSelect+` `+processNodeJoin+` WHERE n.process_id=? ORDER BY n.sequence, n.name`, processID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanProcessNodes(rows)
}

func (db *DB) ListProcessNodesByStation(stationID int64) ([]ProcessNode, error) {
	rows, err := db.Query(`SELECT `+processNodeSelect+` `+processNodeJoin+` WHERE d.operator_station_id=? ORDER BY n.sequence, n.name`, stationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanProcessNodes(rows)
}

func (db *DB) GetProcessNode(id int64) (*ProcessNode, error) {
	n, err := scanProcessNode(db.QueryRow(`SELECT `+processNodeSelect+` `+processNodeJoin+` WHERE n.id=?`, id))
	if err != nil {
		return nil, err
	}
	return &n, nil
}

func (db *DB) CreateProcessNode(in ProcessNodeInput) (int64, error) {
	in = db.normalizeProcessNodeInput(in)
	if in.Code == "" {
		code, err := db.generateProcessNodeCode(in.ProcessID, in.CoreNodeName, in.Name)
		if err != nil {
			return 0, err
		}
		in.Code = code
	}
	if in.Sequence <= 0 {
		next, err := db.nextProcessNodeSequence(in.ProcessID)
		if err != nil {
			return 0, err
		}
		in.Sequence = next
	}
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO process_nodes (
		process_id, code, core_node_name, name, position_type, sequence,
		delivery_node, staging_node, secondary_staging_node,
		staging_node_group, secondary_node_group, full_pickup_node, full_pickup_node_group,
		outgoing_node, outgoing_node_group, allows_reorder, allows_empty_release,
		allows_partial_release, allows_manifest_confirm, allows_station_change, enabled
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.ProcessID, in.Code, in.CoreNodeName, in.Name, in.PositionType, in.Sequence,
		in.DeliveryNode, in.StagingNode, in.SecondaryStagingNode,
		in.StagingNodeGroup, in.SecondaryNodeGroup, in.FullPickupNode, in.FullPickupNodeGroup,
		in.OutgoingNode, in.OutgoingNodeGroup, in.AllowsReorder, in.AllowsEmptyRelease,
		in.AllowsPartialRelease, in.AllowsManifestConfirm, in.AllowsStationChange, in.Enabled,
	)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := upsertStationProcessNodeDelegationTx(tx, id, in.DelegatedStationID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

func (db *DB) UpdateProcessNode(id int64, in ProcessNodeInput) error {
	existing, err := db.GetProcessNode(id)
	if err != nil {
		return err
	}
	in = db.normalizeProcessNodeInput(in)
	if in.Code == "" {
		in.Code = existing.Code
	}
	if in.Sequence <= 0 {
		in.Sequence = existing.Sequence
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`UPDATE process_nodes SET
		process_id=?, code=?, core_node_name=?, name=?, position_type=?, sequence=?,
		delivery_node=?, staging_node=?, secondary_staging_node=?,
		staging_node_group=?, secondary_node_group=?, full_pickup_node=?, full_pickup_node_group=?,
		outgoing_node=?, outgoing_node_group=?, allows_reorder=?, allows_empty_release=?,
		allows_partial_release=?, allows_manifest_confirm=?, allows_station_change=?, enabled=?,
		updated_at=datetime('now')
		WHERE id=?`,
		in.ProcessID, in.Code, in.CoreNodeName, in.Name, in.PositionType, in.Sequence,
		in.DeliveryNode, in.StagingNode, in.SecondaryStagingNode,
		in.StagingNodeGroup, in.SecondaryNodeGroup, in.FullPickupNode, in.FullPickupNodeGroup,
		in.OutgoingNode, in.OutgoingNodeGroup, in.AllowsReorder, in.AllowsEmptyRelease,
		in.AllowsPartialRelease, in.AllowsManifestConfirm, in.AllowsStationChange, in.Enabled,
		id,
	)
	if err != nil {
		return err
	}
	if err := upsertStationProcessNodeDelegationTx(tx, id, in.DelegatedStationID); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) DeleteProcessNode(id int64) error {
	_, err := db.Exec(`DELETE FROM process_nodes WHERE id=?`, id)
	return err
}

func upsertStationProcessNodeDelegationTx(tx *sql.Tx, processNodeID int64, stationID *int64) error {
	if _, err := tx.Exec(`DELETE FROM operator_station_process_nodes WHERE process_node_id=?`, processNodeID); err != nil {
		return err
	}
	if stationID == nil || *stationID <= 0 {
		return nil
	}
	_, err := tx.Exec(`INSERT INTO operator_station_process_nodes (operator_station_id, process_node_id) VALUES (?, ?)`, *stationID, processNodeID)
	return err
}

func (db *DB) normalizeProcessNodeInput(in ProcessNodeInput) ProcessNodeInput {
	in.CoreNodeName = strings.TrimSpace(in.CoreNodeName)
	in.Name = strings.TrimSpace(in.Name)
	in.DeliveryNode = strings.TrimSpace(in.DeliveryNode)
	in.StagingNode = strings.TrimSpace(in.StagingNode)
	in.StagingNodeGroup = strings.TrimSpace(in.StagingNodeGroup)
	in.SecondaryStagingNode = strings.TrimSpace(in.SecondaryStagingNode)
	in.SecondaryNodeGroup = strings.TrimSpace(in.SecondaryNodeGroup)
	in.FullPickupNode = strings.TrimSpace(in.FullPickupNode)
	in.FullPickupNodeGroup = strings.TrimSpace(in.FullPickupNodeGroup)
	in.OutgoingNode = strings.TrimSpace(in.OutgoingNode)
	in.OutgoingNodeGroup = strings.TrimSpace(in.OutgoingNodeGroup)
	if in.Name == "" {
		in.Name = in.CoreNodeName
	}
	if in.PositionType == "" {
		in.PositionType = "consume"
	}
	if in.OutgoingNode == "" && in.CoreNodeName != "" {
		in.OutgoingNode = in.CoreNodeName
	}
	switch in.PositionType {
	case "produce":
		in.AllowsReorder = false
		in.AllowsEmptyRelease = false
		in.AllowsPartialRelease = false
		in.AllowsManifestConfirm = false
		in.AllowsStationChange = false
	default:
		in.PositionType = "consume"
		in.AllowsReorder = true
		in.AllowsEmptyRelease = true
		in.AllowsPartialRelease = true
		in.AllowsManifestConfirm = true
		in.AllowsStationChange = true
	}
	if in.DelegatedStationID != nil && *in.DelegatedStationID <= 0 {
		in.DelegatedStationID = nil
	}
	return in
}

func (db *DB) nextProcessNodeSequence(processID int64) (int, error) {
	var maxSeq sql.NullInt64
	if err := db.QueryRow(`SELECT MAX(sequence) FROM process_nodes WHERE process_id=?`, processID).Scan(&maxSeq); err != nil {
		return 0, err
	}
	if !maxSeq.Valid {
		return 1, nil
	}
	return int(maxSeq.Int64) + 1, nil
}

func (db *DB) generateProcessNodeCode(processID int64, coreNodeName, name string) (string, error) {
	base := slugProcessNodeName(coreNodeName)
	if base == "" {
		base = slugProcessNodeName(name)
	}
	if base == "" {
		base = "node"
	}
	for i := 1; i <= 9999; i++ {
		candidate := base
		if i > 1 {
			candidate = fmt.Sprintf("%s-%d", base, i)
		}
		var exists int
		err := db.QueryRow(`SELECT 1 FROM process_nodes WHERE process_id=? AND code=? LIMIT 1`, processID, candidate).Scan(&exists)
		if err == sql.ErrNoRows {
			return candidate, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("could not generate unique process node code")
}

func slugProcessNodeName(name string) string {
	name = strings.TrimSpace(strings.ToLower(name))
	if name == "" {
		return ""
	}
	var b strings.Builder
	prevDash := false
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			prevDash = false
			continue
		}
		if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
