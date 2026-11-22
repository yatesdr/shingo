package store

// Delegate file for the plant-claims Core mirror (store/plantclaims/).
// Preserves the *store.DB method surface; see plantclaims package docs.

import (
	"time"

	"shingocore/store/plantclaims"
)

// ReplacePlantClaims replaces the mirror for one process from a plant-claims
// message. See plantclaims.ReplaceProcess.
func (db *DB) ReplacePlantClaims(processID string, styles []plantclaims.StyleRow, claims []plantclaims.ClaimRow, staleGuardConfigGen int64) error {
	return plantclaims.ReplaceProcess(db.DB, processID, styles, claims, staleGuardConfigGen)
}

// ReplacePlantClaimsFromStation is ReplacePlantClaims recording the publishing
// station and the report's positions; it returns the station the mirror named
// before and whether the claimed nodes moved (see
// plantclaims.ReplaceProcessFromStation).
func (db *DB) ReplacePlantClaimsFromStation(processID, stationID string, styles []plantclaims.StyleRow, claims []plantclaims.ClaimRow, positions []string, staleGuardConfigGen int64) (plantclaims.Replaced, error) {
	return plantclaims.ReplaceProcessFromStation(db.DB, processID, stationID, styles, claims, positions, staleGuardConfigGen)
}

// ProcessStations maps each process to the station that published it. See
// plantclaims.ProcessStations.
func (db *DB) ProcessStations() (map[string]string, error) {
	return plantclaims.ProcessStations(db.DB)
}

// StationsClaimingNode lists the stations whose processes claim a node. See
// plantclaims.StationsClaimingNode.
func (db *DB) StationsClaimingNode(nodeName string) ([]string, error) {
	return plantclaims.StationsClaimingNode(db.DB, nodeName)
}

// ResolveLandingAddress reads where a landing at nodeName by orderID goes, on q
// (the arrival's transaction). See plantclaims.ResolveLandingAddress.
func (db *DB) ResolveLandingAddress(q plantclaims.Querier, nodeName string, orderID int64) (plantclaims.LandingAddress, error) {
	return plantclaims.ResolveLandingAddress(q, nodeName, orderID)
}

// WipePlantClaims drops every plant-claims mirror row. See plantclaims.WipeAll.
func (db *DB) WipePlantClaims() error {
	return plantclaims.WipeAll(db.DB)
}

// ListProcessNodeOptions returns each process with the Core nodes its claims
// resolve to — what a config editor offers so a person can pick a PROCESS while
// what gets stored is NODES. See plantclaims.ListProcessNodeOptions.
func (db *DB) ListProcessNodeOptions() ([]plantclaims.ProcessNodeOption, error) {
	return plantclaims.ListProcessNodeOptions(db.DB)
}

// ProcessesByNode maps each Core node name to the processes whose setup names
// it. See plantclaims.ProcessesByNode.
func (db *DB) ProcessesByNode() (map[string][]string, error) {
	return plantclaims.ProcessesByNode(db.DB)
}

// RecordPlantClaimsReport writes one station's report row for a process. See
// plantclaims.RecordReport.
func (db *DB) RecordPlantClaimsReport(processID, stationUID, digest string, at time.Time) error {
	return plantclaims.RecordReport(db.DB, processID, stationUID, digest, at)
}

// DeletePlantClaimsReport removes every station's report row for a process and
// returns those stations. See plantclaims.DeleteReport.
func (db *DB) DeletePlantClaimsReport(processID string) ([]string, error) {
	return plantclaims.DeleteReport(db.DB, processID)
}

// PlantClaimsReportDigests returns process → digest for one station's report
// rows. See plantclaims.ReportDigests.
func (db *DB) PlantClaimsReportDigests(stationID string) (map[string]string, error) {
	return plantclaims.ReportDigests(db.DB, stationID)
}

// SharedPlantClaimsReports returns, per process this station and another both
// hold a report row for, every station that holds one. See
// plantclaims.SharedReports.
func (db *DB) SharedPlantClaimsReports(stationUID string) (map[string][]string, error) {
	return plantclaims.SharedReports(db.DB, stationUID)
}

// SharedClaimedNodes lists the physical nodes a station shares with another
// where one side is a claim. See plantclaims.SharedNodes.
func (db *DB) SharedClaimedNodes(stationUID string) ([]plantclaims.SharedNode, error) {
	return plantclaims.SharedNodes(db.DB, stationUID)
}

// NodesClaimedElsewhere maps each physical node other stations claim to those
// stations' uids. See plantclaims.ClaimedElsewhere.
func (db *DB) NodesClaimedElsewhere(stationUID string) (map[string][]string, error) {
	return plantclaims.ClaimedElsewhere(db.DB, stationUID)
}
