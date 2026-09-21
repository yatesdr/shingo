package service

import (
	"shingoedge/domain"
	"shingoedge/store"
	"shingoedge/store/lineside"
	"shingoedge/store/process_groups"
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

// ErrProcessHasStock re-exports the store's refusal so www can classify it
// without importing the store package directly — the `www-no-direct-store`
// depguard rule. The sentinel is the store's; this is a name, not a copy, so
// errors.Is matches either spelling.
var ErrProcessHasStock = processes.ErrProcessHasStock

// ErrDuplicateGroupName re-exports the store's UNIQUE-constraint refusal on
// process_groups.name, for the same www classification pattern.
var ErrDuplicateGroupName = process_groups.ErrDuplicateGroupName

// Delete removes a process row by id, retiring the rows that are meaningless
// without it. Returns ErrProcessHasStock when lineside stock is still booked at
// the process's nodes — a precondition the operator can clear, not a fault.
//
// ITS ONE CALLER IS Engine.DeleteProcess, and a second one would be a bug. This
// is the delete's LAST step: the engine closes the process's open demand
// episodes before reaching here, and calling this directly removes those rows
// without telling Core the episodes ended. The composition cannot live on this
// service — the close writer is on the engine, and engine imports service, not
// the other way round.
func (s *ProcessService) Delete(id int64) error {
	return s.db.DeleteProcess(id)
}

// SetActiveStyle flips the active_style_id for a process. Pass nil to
// clear the active style.
func (s *ProcessService) SetActiveStyle(processID int64, styleID *int64) error {
	return s.db.SetActiveStyle(processID, styleID)
}

// SetChangeoverAutoArm writes the per-process CATID auto-arm mode
// (auto|prompt|off); unknown/empty ⇒ auto.
func (s *ProcessService) SetChangeoverAutoArm(processID int64, mode string) error {
	return s.db.SetChangeoverAutoArm(processID, mode)
}

// SetGroupID assigns a process to a group, or unassigns it (pass nil)
// back to "Ungrouped". Pure UI taxonomy.
func (s *ProcessService) SetGroupID(processID int64, groupID *int64) error {
	return s.db.SetProcessGroupID(processID, groupID)
}

// SetFlowComposerEnabled opens or closes the HMI flow composer for a
// process. Off until the engineer has reviewed the routing set.
func (s *ProcessService) SetFlowComposerEnabled(processID int64, enabled bool) error {
	return s.db.SetFlowComposerEnabled(processID, enabled)
}

// ── Routing set ─────────────────────────────────────────────────────
//
// The nodes a process may route material through that are not its own
// positions (domain/routing_set.go). The store's refusals are re-exported
// here, like ErrProcessHasStock, so www can classify them without importing
// the store package.

var (
	ErrRoutingNodeInUse      = processes.ErrRoutingNodeInUse
	ErrRoutingNodeIsPosition = processes.ErrRoutingNodeIsPosition
	ErrInvalidRoutingRole    = processes.ErrInvalidRoutingRole
	ErrRoutingNodeNotFound   = processes.ErrRoutingNodeNotFound
)

// ListRoutingNodes returns a process's routing set in source / staging /
// destination order, with the per-role style counts the Routing panel shows
// as evidence under a backfilled name.
//
// THE COUNTING VARIANT IS THIS ONE because this method has exactly one
// caller, the panel's GET. Everything else on this path — the composer block,
// the preview, the save — goes to the store's plain read.
func (s *ProcessService) ListRoutingNodes(processID int64) ([]domain.RoutingNode, error) {
	return s.db.ListRoutingNodesWithCounts(processID)
}

// UpsertRoutingNode inserts or updates one (process, node, role) row.
func (s *ProcessService) UpsertRoutingNode(in domain.RoutingNodeInput) (int64, error) {
	return s.db.UpsertRoutingNode(in)
}

// SetRoutingNodeEnabled flips one row's enabled flag; adopting a backfilled
// name is enabled=true.
func (s *ProcessService) SetRoutingNodeEnabled(processID, id int64, enabled bool, calledBy string) error {
	return s.db.SetRoutingNodeEnabled(processID, id, enabled, calledBy)
}

// DeleteRoutingNode removes one row, refusing with ErrRoutingNodeInUse while
// a live claim still routes through the node.
func (s *ProcessService) DeleteRoutingNode(processID, id int64) error {
	return s.db.DeleteRoutingNode(processID, id)
}

// DeriveRoutingNodes re-derives one process's routing set from its live
// claims — a no-op, reported as Skipped, once the flow composer is enabled.
// isUnknown may be nil when Core's node list is not available.
func (s *ProcessService) DeriveRoutingNodes(processID int64, isUnknown func(name string) bool) (domain.RoutingDeriveReport, error) {
	return s.db.DeriveRoutingNodesForProcess(processID, isUnknown)
}

// RoutingSetReport counts the set as it stands, without deriving.
func (s *ProcessService) RoutingSetReport(processID int64, isUnknown func(name string) bool) (domain.RoutingDeriveReport, error) {
	return s.db.RoutingSetReport(processID, isUnknown)
}

// RoutingSet is the report and the rows it describes, from one pass. The
// Routing tab's GET wants both and they are one read of one table.
func (s *ProcessService) RoutingSet(processID int64, isUnknown func(name string) bool) (domain.RoutingDeriveReport, []domain.RoutingNode, error) {
	return s.db.RoutingSet(processID, isUnknown)
}

// PutRoutingNodes writes a whole list of (name, role) rows in the order given,
// adopting rows that already exist and NEVER deleting one the list omits.
//
// ADD-AND-ADOPT, NOT A SET-TO, and the reason is the delete guard. Taking a
// routing name out is refused while a live claim still routes through it
// (ErrRoutingNodeInUse) — a refusal an engineer is meant to see, by name, so
// they can move the flow first. A set-to could only either fail the whole
// write for a row nobody touched or drop a row a flow depends on. Add Process
// only ever adds; the Edit sheet keeps its diff, where a removal is a DELETE.
//
// THE INDEX IS THE SEQUENCE. composer-model's defaultRouting opens a new
// position on the LOWEST sequence of each role, so the order the engineer
// picked names in is the order that decides their defaults.
func (s *ProcessService) PutRoutingNodes(processID int64, rows []domain.RoutingNodeInput, calledBy string) error {
	seq := map[string]int{}
	for i := range rows {
		rows[i].ProcessID = processID
		rows[i].Origin = domain.RoutingOriginEngineer
		rows[i].CalledBy = calledBy
		rows[i].Enabled = true
		rows[i].Sequence = seq[rows[i].Role]
		seq[rows[i].Role]++
	}
	// NAMING A ROW IN THIS LIST IS ADOPTING IT, and the upsert already says so:
	// its ON CONFLICT writes enabled, origin and called_by from the input, so a
	// backfilled row named here comes back on, stamped 'engineer', with the
	// session user on it — the same three columns SetRoutingNodeEnabled would
	// have written, in the statement that is already running. A second write
	// per row would be a second statement on the one SQLite connection for a
	// value the first one just set.
	for _, in := range rows {
		if _, err := s.db.UpsertRoutingNode(in); err != nil {
			return err
		}
	}
	return nil
}

// ── The process part set ────────────────────────────────────────────

// ListProcessPayloads is the stored half of the part set — what the sheet that
// edits the list shows, and the only half a write may touch.
func (s *ProcessService) ListProcessPayloads(processID int64) ([]string, error) {
	return s.db.ListProcessPayloads(processID)
}

// ReplaceProcessPayloads sets the stored half to exactly this list.
func (s *ProcessService) ReplaceProcessPayloads(processID int64, codes []string) error {
	return s.db.ReplaceProcessPayloads(processID, codes)
}

// ── Process groups ──────────────────────────────────────────────────

// ListGroups returns all process_groups ordered by name.
func (s *ProcessService) ListGroups() ([]store.ProcessGroup, error) {
	return s.db.ListProcessGroups()
}

// GetGroup returns one process_group by id.
func (s *ProcessService) GetGroup(id int64) (*store.ProcessGroup, error) {
	return s.db.GetProcessGroup(id)
}

// CreateGroup inserts a new process_group and returns the new id.
func (s *ProcessService) CreateGroup(name, description string) (int64, error) {
	return s.db.CreateProcessGroup(name, description)
}

// UpdateGroup modifies a process_group's name and description.
func (s *ProcessService) UpdateGroup(id int64, name, description string) error {
	return s.db.UpdateProcessGroup(id, name, description)
}

// DeleteGroup removes a process_group. Member processes revert to
// Ungrouped via the explicit transactional UPDATE in the store's DeleteGroup
// — foreign_keys is OFF, so the ON DELETE SET NULL FK never fires.
func (s *ProcessService) DeleteGroup(id int64) error {
	return s.db.DeleteProcessGroup(id)
}

// CountGroupMembers returns how many processes are in a group.
func (s *ProcessService) CountGroupMembers(id int64) (int, error) {
	return s.db.CountProcessGroupMembers(id)
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

// ListLinesideBucketsForNode returns every lineside bucket on a node
// (active and stranded), active rows first. Powers the admin "Lineside
// Buckets" page where engineers clear stuck chips.
func (s *ProcessService) ListLinesideBucketsForNode(processNodeID int64) ([]lineside.Bucket, error) {
	return s.db.ListLinesideBuckets(processNodeID)
}

// UpdateNodeRuntimeOrders writes the active and staged order ids on
// the runtime row.
func (s *ProcessService) UpdateNodeRuntimeOrders(processNodeID int64, activeOrderID, stagedOrderID *int64) error {
	return s.db.UpdateProcessNodeRuntimeOrders(processNodeID, activeOrderID, stagedOrderID)
}

// ClearNodeRuntimeOrders drops both order pointers on one node. Named so the
// operator clear reads as the deliberate wipe it is, rather than as a two-column
// write that happens to pass nil twice.
func (s *ProcessService) ClearNodeRuntimeOrders(processNodeID int64) error {
	return s.db.ClearProcessNodeRuntimeOrders(processNodeID)
}
