package service

import (
	"shingoedge/store"
	"shingoedge/store/processes"
)

// ProcessService owns the process aggregate's CRUD: processes
// themselves, process_nodes, and process_node_runtime_states. These
// three tables together describe the production line's structure (a
// process has many nodes; each node has runtime state).
//
// Style transitions (SetActiveStyle) live on this service because
// they're a process-level concern — a process *runs* a style, and
// flipping the active style is a process operation.
//
// Phase 6.2′ extracted this from named methods on *engine.Engine.
type ProcessService struct {
	db *store.DB
}

// NewProcessService constructs a ProcessService wrapping the shared
// *store.DB.
func NewProcessService(db *store.DB) *ProcessService {
	return &ProcessService{db: db}
}

// ── Processes ──────────────────────────────────────────────────────

// List returns all processes ordered by name.
func (s *ProcessService) List() ([]processes.Process, error) {
	return s.db.ListProcesses()
}

// Create inserts a new process and returns the new row id.
func (s *ProcessService) Create(name, description, productionState, counterPLC, counterTag string, counterEnabled bool) (int64, error) {
	return s.db.CreateProcess(name, description, productionState, counterPLC, counterTag, counterEnabled)
}

// Update modifies an existing process.
func (s *ProcessService) Update(id int64, name, description, productionState, counterPLC, counterTag string, counterEnabled bool) error {
	return s.db.UpdateProcess(id, name, description, productionState, counterPLC, counterTag, counterEnabled)
}

// Delete removes a process row by id.
func (s *ProcessService) Delete(id int64) error {
	return s.db.DeleteProcess(id)
}

// SetActiveStyle flips the active_style_id for a process. Pass nil to
// clear the active style.
func (s *ProcessService) SetActiveStyle(processID int64, styleID *int64) error {
	return s.db.SetActiveStyle(processID, styleID)
}

// ── Process nodes ──────────────────────────────────────────────────

// ListNodes returns every process_nodes row.
func (s *ProcessService) ListNodes() ([]processes.Node, error) {
	return s.db.ListProcessNodes()
}

// ListNodesByProcess returns nodes owned by a single process.
func (s *ProcessService) ListNodesByProcess(processID int64) ([]processes.Node, error) {
	return s.db.ListProcessNodesByProcess(processID)
}

// ListNodesByStation returns nodes assigned to an operator station.
func (s *ProcessService) ListNodesByStation(stationID int64) ([]processes.Node, error) {
	return s.db.ListProcessNodesByStation(stationID)
}

// GetNode returns one process_node by id.
func (s *ProcessService) GetNode(id int64) (*processes.Node, error) {
	return s.db.GetProcessNode(id)
}

// CreateNode inserts a new process_node and returns the new row id.
func (s *ProcessService) CreateNode(in processes.NodeInput) (int64, error) {
	return s.db.CreateProcessNode(in)
}

// UpdateNode modifies a process_node.
func (s *ProcessService) UpdateNode(id int64, in processes.NodeInput) error {
	return s.db.UpdateProcessNode(id, in)
}

// DeleteNode removes a process_node row by id.
func (s *ProcessService) DeleteNode(id int64) error {
	return s.db.DeleteProcessNode(id)
}

// ── Process node runtime ──────────────────────────────────────────

// EnsureNodeRuntime returns the runtime row for a process_node,
// inserting a fresh row when none exists yet.
func (s *ProcessService) EnsureNodeRuntime(processNodeID int64) (*processes.RuntimeState, error) {
	return s.db.EnsureProcessNodeRuntime(processNodeID)
}

// UpdateNodeRuntimeOrders writes the active and staged order ids on
// the runtime row.
func (s *ProcessService) UpdateNodeRuntimeOrders(processNodeID int64, activeOrderID, stagedOrderID *int64) error {
	return s.db.UpdateProcessNodeRuntimeOrders(processNodeID, activeOrderID, stagedOrderID)
}
