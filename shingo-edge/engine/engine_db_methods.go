// engine_db_methods.go — named delegating methods that forward to *store.DB.
//
// Phase 4 of the shingo-edge/www refactor eliminated every direct
// `h.engine.DB().X(...)` call site by routing handlers through these named
// methods on *engine.Engine instead. The www/engine_iface.go EngineAccess
// interface declares the same surface so test doubles can satisfy it without
// touching *store.DB.
//
// Each method here is a one-line passthrough by design: a future PR will
// extract these into per-domain services (style/process/operator-station/
// changeover/etc.), at which point the named methods either move to those
// services or remain as thin engine-level façades. Until that work lands,
// this file is the long-term home for the edge-side db surface used by
// www handlers.
//
// See docs/edge-named-methods.md for the per-call-site traceability table
// produced when this layer was created.

package engine

import (
	"shingoedge/store"
)

func (e *Engine) AdminUserExists() (bool, error) {
	return e.db.AdminUserExists()
}

func (e *Engine) BuildOperatorStationView(stationID int64) (*store.OperatorStationView, error) {
	return e.db.BuildOperatorStationView(stationID)
}

func (e *Engine) ConfirmAnomaly(id int64) error {
	return e.db.ConfirmAnomaly(id)
}

func (e *Engine) CreateAdminUser(username, passwordHash string) (int64, error) {
	return e.db.CreateAdminUser(username, passwordHash)
}

func (e *Engine) CreateOperatorStation(in store.OperatorStationInput) (int64, error) {
	return e.db.CreateOperatorStation(in)
}

func (e *Engine) CreateProcess(name, description, productionState, counterPLC, counterTag string, counterEnabled bool) (int64, error) {
	return e.db.CreateProcess(name, description, productionState, counterPLC, counterTag, counterEnabled)
}

func (e *Engine) CreateProcessNode(in store.ProcessNodeInput) (int64, error) {
	return e.db.CreateProcessNode(in)
}

func (e *Engine) CreateReportingPoint(plcName, tagName string, styleID int64) (int64, error) {
	return e.db.CreateReportingPoint(plcName, tagName, styleID)
}

func (e *Engine) CreateStyle(name, description string, processID int64) (int64, error) {
	return e.db.CreateStyle(name, description, processID)
}

func (e *Engine) DeleteOperatorStation(id int64) error {
	return e.db.DeleteOperatorStation(id)
}

func (e *Engine) DeleteProcess(id int64) error {
	return e.db.DeleteProcess(id)
}

func (e *Engine) DeleteProcessNode(id int64) error {
	return e.db.DeleteProcessNode(id)
}

func (e *Engine) DeleteReportingPoint(id int64) error {
	return e.db.DeleteReportingPoint(id)
}

func (e *Engine) DeleteShift(shiftNumber int) error {
	return e.db.DeleteShift(shiftNumber)
}

func (e *Engine) DeleteStyle(id int64) error {
	return e.db.DeleteStyle(id)
}

func (e *Engine) DeleteStyleNodeClaim(id int64) error {
	return e.db.DeleteStyleNodeClaim(id)
}

func (e *Engine) DismissAnomaly(id int64) error {
	return e.db.DismissAnomaly(id)
}

func (e *Engine) EnsureProcessNodeRuntime(processNodeID int64) (*store.ProcessNodeRuntimeState, error) {
	return e.db.EnsureProcessNodeRuntime(processNodeID)
}

func (e *Engine) GetActiveProcessChangeover(processID int64) (*store.ProcessChangeover, error) {
	return e.db.GetActiveProcessChangeover(processID)
}

func (e *Engine) GetAdminUser(username string) (*store.AdminUser, error) {
	return e.db.GetAdminUser(username)
}

func (e *Engine) GetOperatorStation(id int64) (*store.OperatorStation, error) {
	return e.db.GetOperatorStation(id)
}

func (e *Engine) GetOrder(id int64) (*store.Order, error) {
	return e.db.GetOrder(id)
}

func (e *Engine) GetProcessNode(id int64) (*store.ProcessNode, error) {
	return e.db.GetProcessNode(id)
}

func (e *Engine) GetReportingPoint(id int64) (*store.ReportingPoint, error) {
	return e.db.GetReportingPoint(id)
}

func (e *Engine) GetStationNodeNames(stationID int64) ([]string, error) {
	return e.db.GetStationNodeNames(stationID)
}

func (e *Engine) GetStyle(id int64) (*store.Style, error) {
	return e.db.GetStyle(id)
}

func (e *Engine) GetStyleNodeClaim(id int64) (*store.StyleNodeClaim, error) {
	return e.db.GetStyleNodeClaim(id)
}

func (e *Engine) HourlyCountTotals(processID int64, countDate string) (map[int]int64, error) {
	return e.db.HourlyCountTotals(processID, countDate)
}

func (e *Engine) ListActiveOrders() ([]store.Order, error) {
	return e.db.ListActiveOrders()
}

func (e *Engine) ListActiveOrdersByProcess(processID int64) ([]store.Order, error) {
	return e.db.ListActiveOrdersByProcess(processID)
}

func (e *Engine) ListChangeoverNodeTasks(changeoverID int64) ([]store.ChangeoverNodeTask, error) {
	return e.db.ListChangeoverNodeTasks(changeoverID)
}

func (e *Engine) ListChangeoverStationTasks(changeoverID int64) ([]store.ChangeoverStationTask, error) {
	return e.db.ListChangeoverStationTasks(changeoverID)
}

func (e *Engine) ListHourlyCounts(processID, styleID int64, countDate string) ([]store.HourlyCount, error) {
	return e.db.ListHourlyCounts(processID, styleID, countDate)
}

func (e *Engine) ListOperatorStations() ([]store.OperatorStation, error) {
	return e.db.ListOperatorStations()
}

func (e *Engine) ListOperatorStationsByProcess(processID int64) ([]store.OperatorStation, error) {
	return e.db.ListOperatorStationsByProcess(processID)
}

func (e *Engine) ListPayloadCatalog() ([]*store.PayloadCatalogEntry, error) {
	return e.db.ListPayloadCatalog()
}

func (e *Engine) ListProcessChangeovers(processID int64) ([]store.ProcessChangeover, error) {
	return e.db.ListProcessChangeovers(processID)
}

func (e *Engine) ListProcessNodes() ([]store.ProcessNode, error) {
	return e.db.ListProcessNodes()
}

func (e *Engine) ListProcessNodesByProcess(processID int64) ([]store.ProcessNode, error) {
	return e.db.ListProcessNodesByProcess(processID)
}

func (e *Engine) ListProcessNodesByStation(stationID int64) ([]store.ProcessNode, error) {
	return e.db.ListProcessNodesByStation(stationID)
}

func (e *Engine) ListProcesses() ([]store.Process, error) {
	return e.db.ListProcesses()
}

func (e *Engine) ListReportingPoints() ([]store.ReportingPoint, error) {
	return e.db.ListReportingPoints()
}

func (e *Engine) ListShifts() ([]store.Shift, error) {
	return e.db.ListShifts()
}

func (e *Engine) ListStyleNodeClaims(styleID int64) ([]store.StyleNodeClaim, error) {
	return e.db.ListStyleNodeClaims(styleID)
}

func (e *Engine) ListStyles() ([]store.Style, error) {
	return e.db.ListStyles()
}

func (e *Engine) ListStylesByProcess(processID int64) ([]store.Style, error) {
	return e.db.ListStylesByProcess(processID)
}

func (e *Engine) ListUnconfirmedAnomalies() ([]store.CounterSnapshot, error) {
	return e.db.ListUnconfirmedAnomalies()
}

func (e *Engine) MoveOperatorStation(id int64, direction string) error {
	return e.db.MoveOperatorStation(id, direction)
}

func (e *Engine) SetActiveStyle(processID int64, styleID *int64) error {
	return e.db.SetActiveStyle(processID, styleID)
}

func (e *Engine) SetStationNodes(stationID int64, nodeNames []string) error {
	return e.db.SetStationNodes(stationID, nodeNames)
}

func (e *Engine) TouchOperatorStation(id int64, healthStatus string) error {
	return e.db.TouchOperatorStation(id, healthStatus)
}

func (e *Engine) UpdateAdminPassword(username, passwordHash string) error {
	return e.db.UpdateAdminPassword(username, passwordHash)
}

func (e *Engine) UpdateOperatorStation(id int64, in store.OperatorStationInput) error {
	return e.db.UpdateOperatorStation(id, in)
}

func (e *Engine) UpdateOrderFinalCount(id int64, finalCount int64, confirmed bool) error {
	return e.db.UpdateOrderFinalCount(id, finalCount, confirmed)
}

func (e *Engine) UpdateProcess(id int64, name, description, productionState, counterPLC, counterTag string, counterEnabled bool) error {
	return e.db.UpdateProcess(id, name, description, productionState, counterPLC, counterTag, counterEnabled)
}

func (e *Engine) UpdateProcessNode(id int64, in store.ProcessNodeInput) error {
	return e.db.UpdateProcessNode(id, in)
}

func (e *Engine) UpdateProcessNodeRuntimeOrders(processNodeID int64, activeOrderID, stagedOrderID *int64) error {
	return e.db.UpdateProcessNodeRuntimeOrders(processNodeID, activeOrderID, stagedOrderID)
}

func (e *Engine) UpdateReportingPoint(id int64, plcName, tagName string, styleID int64, enabled bool) error {
	return e.db.UpdateReportingPoint(id, plcName, tagName, styleID, enabled)
}

func (e *Engine) UpdateStyle(id int64, name, description string, processID int64) error {
	return e.db.UpdateStyle(id, name, description, processID)
}

func (e *Engine) UpsertShift(shiftNumber int, name, startTime, endTime string) error {
	return e.db.UpsertShift(shiftNumber, name, startTime, endTime)
}

func (e *Engine) UpsertStyleNodeClaim(in store.StyleNodeClaimInput) (int64, error) {
	return e.db.UpsertStyleNodeClaim(in)
}
