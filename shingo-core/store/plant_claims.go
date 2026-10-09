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

// WipePlantClaims drops every plant-claims mirror row. See plantclaims.WipeAll.
func (db *DB) WipePlantClaims() error {
	return plantclaims.WipeAll(db.DB)
}

// PlantClaimsDirtyIndex builds the payload → (process, style) dirty index from
// the mirror. See plantclaims.DirtyIndex.
func (db *DB) PlantClaimsDirtyIndex() (map[string][]plantclaims.ProcessKey, error) {
	return plantclaims.DirtyIndex(db.DB)
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
