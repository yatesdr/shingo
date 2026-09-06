package engine

import (
	"shingocore/config"
	"shingocore/dispatch"
	"shingocore/dispatch/eta"
	"shingocore/fleet"
	"shingocore/messaging"
	"shingocore/notify"
	"shingocore/service"
	"shingocore/store"
)

// ── Accessors ───────────────────────────────────────────────────────
//
// One-liner getters for the subsystems and services held by Engine.
// Kept as accessors rather than exported fields so call sites in
// www/, cmd/, and test packages bind to the method contract rather
// than the concrete struct layout.

func (e *Engine) DB() *store.DB                            { return e.db }
func (e *Engine) AppConfig() *config.Config                { return e.cfg }
func (e *Engine) ConfigPath() string                       { return e.configPath }
func (e *Engine) Dispatcher() *dispatch.Dispatcher         { return e.dispatcher }
func (e *Engine) Tracker() fleet.OrderTracker              { return e.tracker }
func (e *Engine) Fleet() fleet.Backend                     { return e.fleet }
func (e *Engine) MsgClient() *messaging.Client             { return e.msgClient }
func (e *Engine) Reconciliation() *ReconciliationService   { return e.reconciliation }
func (e *Engine) Recovery() *RecoveryService               { return e.recovery }
func (e *Engine) BinManifest() *service.BinManifestService { return e.binManifest }
func (e *Engine) BinService() *service.BinService          { return e.binService }
func (e *Engine) OrderService() *service.OrderService      { return e.orderService }
func (e *Engine) NodeService() *service.NodeService        { return e.nodeService }
func (e *Engine) AuditService() *service.AuditService      { return e.auditService }
func (e *Engine) DemandService() *service.DemandService    { return e.demandService }

func (e *Engine) DemandEpisodeService() *service.DemandEpisodeService {
	return e.demandEpisodeService
}
func (e *Engine) LoaderService() *service.LoaderService { return e.loaderService }
func (e *Engine) CalculatorService() *service.ThresholdCalculatorService {
	return e.calculatorService
}
func (e *Engine) PayloadService() *service.PayloadService         { return e.payloadService }
func (e *Engine) MissionService() *service.MissionService         { return e.missionService }
func (e *Engine) TestCommandService() *service.TestCommandService { return e.testCmdService }
func (e *Engine) CMSTransactionService() *service.CMSTransactionService {
	return e.cmsTxnService
}

// CMSFeedHealth answers whether the middleware feed is working.
//
// It lives on the Engine rather than purely on the service because two of the
// three inputs are PROCESS state, not database state: whether a cms: block was
// configured, and whether the poster has muted itself. A health answer built
// from the tables alone would report a muted poster with a quiet queue as
// healthy and idle.
func (e *Engine) CMSFeedHealth() (*service.FeedHealth, error) {
	ps := service.ProcessState{Enabled: e.cfg.CMS.Enabled()}
	if e.cmsPoster != nil {
		ps.Muted, ps.MutedReason = e.cmsPoster.Muted(), e.cmsPoster.MutedReason()
	}
	h, err := e.cmsPostingService.Health(ps)
	if err != nil {
		return nil, err
	}
	// The build-failure counter. A movement whose transaction rows could not be
	// built never reaches cms_postings at all, so no query over that table can
	// see it — this is the only place the loss is visible.
	h.BuildFailures = e.cmsBuildFailures.Load()
	h.Verdict()
	return h, nil
}
func (e *Engine) InventoryService() *service.InventoryService { return e.inventoryService }
func (e *Engine) AdminService() *service.AdminService         { return e.adminService }
func (e *Engine) HealthService() *service.HealthService       { return e.healthService }
func (e *Engine) TagVerifyService() *service.TagVerifyService { return e.tagVerifyService }
func (e *Engine) InventoryDeltaService() *service.InventoryDeltaService {
	return e.inventoryDeltaService
}
func (e *Engine) DashboardService() *service.DashboardService { return e.dashboardService }
func (e *Engine) FootprintService() *service.FootprintService { return e.footprintService }
func (e *Engine) PartsService() *service.PartsService         { return e.partsService }
func (e *Engine) HeartbeatService() *service.HeartbeatService { return e.heartbeatService }
func (e *Engine) EventBus() *EventBus                         { return e.Events }
func (e *Engine) EtaCache() *eta.Cache                        { return e.etaCache }
func (e *Engine) Notifier() *notify.Notifier                  { return e.notifier }

// Maintainer returns the maintained-group level keeper, for the health page.
func (e *Engine) Maintainer() *Maintainer { return e.maintainer }
