package engine

import (
	"shingocore/config"
	"shingocore/countgroup"
	"shingocore/dispatch"
	"shingocore/fleet"
	"shingocore/messaging"
	"shingocore/service"
	"shingocore/store"
)

// ── Accessors ───────────────────────────────────────────────────────
//
// One-liner getters for the subsystems and services held by Engine.
// Kept as accessors rather than exported fields so call sites in
// www/, cmd/, and test packages bind to the method contract rather
// than the concrete struct layout.

func (e *Engine) DB() *store.DB                             { return e.db }
func (e *Engine) AppConfig() *config.Config                 { return e.cfg }
func (e *Engine) ConfigPath() string                        { return e.configPath }
func (e *Engine) Dispatcher() *dispatch.Dispatcher          { return e.dispatcher }
func (e *Engine) Tracker() fleet.OrderTracker               { return e.tracker }
func (e *Engine) Fleet() fleet.Backend                      { return e.fleet }
func (e *Engine) MsgClient() *messaging.Client              { return e.msgClient }
func (e *Engine) Reconciliation() *ReconciliationService    { return e.reconciliation }
func (e *Engine) Recovery() *RecoveryService                { return e.recovery }
func (e *Engine) BinManifest() *service.BinManifestService  { return e.binManifest }
func (e *Engine) BinService() *service.BinService           { return e.binService }
func (e *Engine) OrderService() *service.OrderService       { return e.orderService }
func (e *Engine) NodeService() *service.NodeService         { return e.nodeService }
func (e *Engine) EventBus() *EventBus                       { return e.Events }

// SetCountGroupRunner registers a configured Runner built by the
// composition root. The caller passes the Runner directly — transitions
// land on the engine's EventBus via the internal emitter adapter.
// Engine.Start() will call .Start() on it; Engine.Stop() will call .Stop().
// Pass nil (or just don't call) to disable the feature.
//
// Takes a factory function that receives the EventBus-backed emitter so
// the caller can build the Runner without the engine exposing emitter
// construction as part of its public API.
func (e *Engine) SetCountGroupRunner(build func(countgroup.Emitter) *countgroup.Runner) {
	if build == nil {
		return
	}
	e.countGroupBuild = build
	e.countGroup = build(&countGroupEventEmitter{bus: e.Events})
}
