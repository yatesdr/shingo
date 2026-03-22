package engine

import (
	"context"
	"database/sql"
	"log"
	"time"
)

// EnsureTagPublished enables WarLink tag publishing if the tag is not already
// published, and marks the reporting point as warlink-managed.
func (e *Engine) EnsureTagPublished(rpID int64, plcName, tagName string) {
	mgr := e.plcMgr
	if mgr.IsTagPublished(plcName, tagName) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := mgr.EnableTagPublishing(ctx, plcName, tagName); err != nil {
		log.Printf("warlink: auto-enable %s/%s failed (RP %d): %v", plcName, tagName, rpID, err)
		e.debugFn.Log("warlink: auto-enable %s/%s failed (RP %d): %v", plcName, tagName, rpID, err)
		return
	}
	if err := e.db.SetReportingPointManaged(rpID, true); err != nil {
		log.Printf("warlink: set RP %d managed: %v", rpID, err)
	}
}

// EnsureTagUnpublished disables WarLink tag publishing for a warlink-managed
// reporting point.
func (e *Engine) EnsureTagUnpublished(rpID int64, plcName, tagName string) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := e.plcMgr.DisableTagPublishing(ctx, plcName, tagName); err != nil {
		log.Printf("warlink: auto-disable %s/%s failed (RP %d): %v", plcName, tagName, rpID, err)
		e.debugFn.Log("warlink: auto-disable %s/%s failed (RP %d): %v", plcName, tagName, rpID, err)
	}
}

// ManageReportingPointTag handles WarLink tag lifecycle when a reporting point's
// PLC/tag assignment changes: disables the old tag (if warlink-managed) and
// enables the new one.
func (e *Engine) ManageReportingPointTag(rpID int64, oldPLC, oldTag string, oldManaged bool, newPLC, newTag string) {
	if oldPLC == newPLC && oldTag == newTag {
		return // no change
	}

	// Step 1: Disable old tag if we were managing it.
	if oldManaged {
		e.EnsureTagUnpublished(rpID, oldPLC, oldTag)
	}

	// Step 2: Enable new tag if specified.
	if newPLC == "" || newTag == "" {
		return
	}

	// If already published externally, mark as not managed by us.
	if e.plcMgr.IsTagPublished(newPLC, newTag) {
		if err := e.db.SetReportingPointManaged(rpID, false); err != nil {
			log.Printf("warlink: clear RP %d managed (already published): %v", rpID, err)
		}
		return
	}

	// Attempt to enable and mark as managed.
	e.enableAndMarkManaged(rpID, newPLC, newTag)
}

// enableAndMarkManaged enables WarLink tag publishing and sets the reporting
// point's managed flag. On failure, clears the managed flag.
func (e *Engine) enableAndMarkManaged(rpID int64, plcName, tagName string) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := e.plcMgr.EnableTagPublishing(ctx, plcName, tagName); err != nil {
		log.Printf("warlink: auto-enable %s/%s failed (RP %d): %v", plcName, tagName, rpID, err)
		e.debugFn.Log("warlink: auto-enable %s/%s failed (RP %d): %v", plcName, tagName, rpID, err)
		if err := e.db.SetReportingPointManaged(rpID, false); err != nil {
			log.Printf("warlink: clear RP %d managed: %v", rpID, err)
		}
		return
	}
	if err := e.db.SetReportingPointManaged(rpID, true); err != nil {
		log.Printf("warlink: set RP %d managed: %v", rpID, err)
	}
}

// CleanupReportingPointTag disables WarLink publishing for a deleted reporting
// point if it was warlink-managed.
func (e *Engine) CleanupReportingPointTag(rpID int64, plcName, tagName string, managed bool) {
	if managed {
		e.EnsureTagUnpublished(rpID, plcName, tagName)
	}
}

func (e *Engine) SyncProcessCounterBinding(processID int64) error {
	process, err := e.db.GetProcess(processID)
	if err != nil {
		return err
	}
	binding, err := e.db.GetProcessCounterBinding(processID)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	if binding.PLCName == "" || binding.TagName == "" {
		return nil
	}
	if process.ActiveStyleID == nil {
		return nil
	}

	var oldPLC, oldTag string
	var oldManaged bool
	if binding.ReportingPointID != nil {
		if rp, err := e.db.GetReportingPoint(*binding.ReportingPointID); err == nil {
			oldPLC, oldTag, oldManaged = rp.PLCName, rp.TagName, rp.WarlinkManaged
			if err := e.db.UpdateReportingPoint(rp.ID, binding.PLCName, binding.TagName, *process.ActiveStyleID, binding.Enabled); err != nil {
				return err
			}
			e.ManageReportingPointTag(rp.ID, oldPLC, oldTag, oldManaged, binding.PLCName, binding.TagName)
			rpID := rp.ID
			return e.db.UpdateProcessCounterReportingPoint(processID, &rpID, oldManaged || e.plcMgr.IsTagPublished(binding.PLCName, binding.TagName))
		}
	}

	rpID, err := e.db.CreateReportingPoint(binding.PLCName, binding.TagName, *process.ActiveStyleID)
	if err != nil {
		return err
	}
	if !binding.Enabled {
		_ = e.db.UpdateReportingPoint(rpID, binding.PLCName, binding.TagName, *process.ActiveStyleID, false)
	}
	e.EnsureTagPublished(rpID, binding.PLCName, binding.TagName)
	managed := false
	if rp, err := e.db.GetReportingPoint(rpID); err == nil {
		managed = rp.WarlinkManaged
	}
	return e.db.UpdateProcessCounterReportingPoint(processID, &rpID, managed)
}
