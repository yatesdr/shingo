package store

import "time"

type OpStationNode struct {
	ID                     int64     `json:"id"`
	OperatorStationID      int64     `json:"operator_station_id"`
	Code                   string    `json:"code"`
	Name                   string    `json:"name"`
	PositionType           string    `json:"position_type"`
	Sequence               int       `json:"sequence"`
	DeliveryNode           string    `json:"delivery_node"`
	StagingNode            string    `json:"staging_node"`
	SecondaryStagingNode   string    `json:"secondary_staging_node"`
	StagingNodeGroup       string    `json:"staging_node_group"`
	SecondaryNodeGroup     string    `json:"secondary_node_group"`
	FullPickupNode         string    `json:"full_pickup_node"`
	FullPickupNodeGroup    string    `json:"full_pickup_node_group"`
	OutgoingNode           string    `json:"outgoing_node"`
	OutgoingNodeGroup      string    `json:"outgoing_node_group"`
	AllowsReorder          bool      `json:"allows_reorder"`
	AllowsEmptyRelease     bool      `json:"allows_empty_release"`
	AllowsPartialRelease   bool      `json:"allows_partial_release"`
	AllowsManifestConfirm  bool      `json:"allows_manifest_confirm"`
	AllowsStationChange    bool      `json:"allows_station_change"`
	Enabled                bool      `json:"enabled"`
	CreatedAt              time.Time `json:"created_at"`
	UpdatedAt              time.Time `json:"updated_at"`

	StationName string `json:"station_name"`
	ProcessID   int64  `json:"process_id"`
	ProcessName string `json:"process_name"`
}

type OpStationNodeInput struct {
	OperatorStationID     int64  `json:"operator_station_id"`
	Code                  string `json:"code"`
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

const opNodeSelect = `n.id, n.operator_station_id, n.code, n.name, n.position_type, n.sequence,
	n.delivery_node, n.staging_node, n.secondary_staging_node,
	n.staging_node_group, n.secondary_node_group, n.full_pickup_node,
	n.full_pickup_node_group, n.outgoing_node, n.outgoing_node_group,
	n.allows_reorder, n.allows_empty_release, n.allows_partial_release,
	n.allows_manifest_confirm, n.allows_station_change, n.enabled,
	n.created_at, n.updated_at, COALESCE(s.name, ''), COALESCE(s.process_id, 0), COALESCE(p.name, '')`

const opNodeJoin = `FROM op_station_nodes n
	LEFT JOIN operator_stations s ON s.id = n.operator_station_id
	LEFT JOIN processes p ON p.id = s.process_id`

func scanOpNode(scanner interface{ Scan(...interface{}) error }) (OpStationNode, error) {
	var n OpStationNode
	var createdAt, updatedAt string
	err := scanner.Scan(
		&n.ID, &n.OperatorStationID, &n.Code, &n.Name, &n.PositionType, &n.Sequence,
		&n.DeliveryNode, &n.StagingNode, &n.SecondaryStagingNode,
		&n.StagingNodeGroup, &n.SecondaryNodeGroup, &n.FullPickupNode,
		&n.FullPickupNodeGroup, &n.OutgoingNode, &n.OutgoingNodeGroup,
		&n.AllowsReorder, &n.AllowsEmptyRelease, &n.AllowsPartialRelease,
		&n.AllowsManifestConfirm, &n.AllowsStationChange, &n.Enabled,
		&createdAt, &updatedAt, &n.StationName, &n.ProcessID, &n.ProcessName,
	)
	if err != nil {
		return n, err
	}
	n.CreatedAt = scanTime(createdAt)
	n.UpdatedAt = scanTime(updatedAt)
	return n, nil
}

func scanOpNodes(rows rowScanner) ([]OpStationNode, error) {
	var out []OpStationNode
	for rows.Next() {
		n, err := scanOpNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (db *DB) ListOpStationNodes() ([]OpStationNode, error) {
	rows, err := db.Query(`SELECT ` + opNodeSelect + ` ` + opNodeJoin + ` ORDER BY s.process_id, s.sequence, n.sequence, n.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOpNodes(rows)
}

func (db *DB) ListOpStationNodesByStation(stationID int64) ([]OpStationNode, error) {
	rows, err := db.Query(`SELECT `+opNodeSelect+` `+opNodeJoin+` WHERE n.operator_station_id=? ORDER BY n.sequence, n.name`, stationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOpNodes(rows)
}

func (db *DB) ListOpStationNodesByProcess(processID int64) ([]OpStationNode, error) {
	rows, err := db.Query(`SELECT `+opNodeSelect+` `+opNodeJoin+` WHERE s.process_id=? ORDER BY s.sequence, n.sequence, n.name`, processID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOpNodes(rows)
}

func (db *DB) GetOpStationNode(id int64) (*OpStationNode, error) {
	n, err := scanOpNode(db.QueryRow(`SELECT `+opNodeSelect+` `+opNodeJoin+` WHERE n.id=?`, id))
	if err != nil {
		return nil, err
	}
	return &n, nil
}

func (db *DB) CreateOpStationNode(in OpStationNodeInput) (int64, error) {
	res, err := db.Exec(`INSERT INTO op_station_nodes (
		operator_station_id, code, name, position_type, sequence,
		delivery_node, staging_node, secondary_staging_node,
		staging_node_group, secondary_node_group, full_pickup_node, full_pickup_node_group,
		outgoing_node, outgoing_node_group, allows_reorder, allows_empty_release,
		allows_partial_release, allows_manifest_confirm, allows_station_change, enabled
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.OperatorStationID, in.Code, in.Name, in.PositionType, in.Sequence,
		in.DeliveryNode, in.StagingNode, in.SecondaryStagingNode,
		in.StagingNodeGroup, in.SecondaryNodeGroup, in.FullPickupNode, in.FullPickupNodeGroup,
		in.OutgoingNode, in.OutgoingNodeGroup, in.AllowsReorder, in.AllowsEmptyRelease,
		in.AllowsPartialRelease, in.AllowsManifestConfirm, in.AllowsStationChange, in.Enabled,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) UpdateOpStationNode(id int64, in OpStationNodeInput) error {
	_, err := db.Exec(`UPDATE op_station_nodes SET
		operator_station_id=?, code=?, name=?, position_type=?, sequence=?,
		delivery_node=?, staging_node=?, secondary_staging_node=?,
		staging_node_group=?, secondary_node_group=?, full_pickup_node=?, full_pickup_node_group=?,
		outgoing_node=?, outgoing_node_group=?, allows_reorder=?, allows_empty_release=?,
		allows_partial_release=?, allows_manifest_confirm=?, allows_station_change=?, enabled=?,
		updated_at=datetime('now')
		WHERE id=?`,
		in.OperatorStationID, in.Code, in.Name, in.PositionType, in.Sequence,
		in.DeliveryNode, in.StagingNode, in.SecondaryStagingNode,
		in.StagingNodeGroup, in.SecondaryNodeGroup, in.FullPickupNode, in.FullPickupNodeGroup,
		in.OutgoingNode, in.OutgoingNodeGroup, in.AllowsReorder, in.AllowsEmptyRelease,
		in.AllowsPartialRelease, in.AllowsManifestConfirm, in.AllowsStationChange, in.Enabled,
		id,
	)
	return err
}

func (db *DB) DeleteOpStationNode(id int64) error {
	_, err := db.Exec(`DELETE FROM op_station_nodes WHERE id=?`, id)
	return err
}
