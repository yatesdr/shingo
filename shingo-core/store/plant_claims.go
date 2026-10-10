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
// station; it returns the station the mirror named before (see
// plantclaims.ReplaceProcessFromStation).
func (db *DB) ReplacePlantClaimsFromStation(processID, stationID string, styles []plantclaims.StyleRow, claims []plantclaims.ClaimRow, staleGuardConfigGen int64) (string, error) {
	return plantclaims.ReplaceProcessFromStation(db.DB, processID, stationID, styles, claims, staleGuardConfigGen)
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

// RecordPlantClaimsReport writes a process's report row (station, digest) and
// returns the station that held it before. See plantclaims.RecordReport.
func (db *DB) RecordPlantClaimsReport(processID, stationID, digest string, at time.Time) (string, error) {
	return plantclaims.RecordReport(db.DB, processID, stationID, digest, at)
}

// DeletePlantClaimsReport removes a process's report row and returns the
// station that held it. See plantclaims.DeleteReport.
func (db *DB) DeletePlantClaimsReport(processID string) (string, error) {
	return plantclaims.DeleteReport(db.DB, processID)
}

// PlantClaimsReportDigests returns process → digest for one station's report
// rows. See plantclaims.ReportDigests.
func (db *DB) PlantClaimsReportDigests(stationID string) (map[string]string, error) {
	return plantclaims.ReportDigests(db.DB, stationID)
}
