// process_counter_edit.go — the process's counter and part-identity edit.
//
// A process counts on one PLC tag (its reporting point) and reads its part
// identity from another (the CATID monitor). The Settings save edits both
// through EditProcessCounter, which:
//
//   - holds the changeover door's lock and refuses while a changeover is open
//     (a re-point mid-changeover would move the count the cutover is about to
//     attribute);
//   - refuses a counter tag another process counts on, or one WarLink does not
//     declare as an integer (the poll truncates a REAL's JSON float);
//   - re-points the reporting point that read the old tag (syncPrimaryPoint);
//   - and, when the identity tag changed, PATCHes WarLink outside every lock
//     and re-primes the monitor's state for the process.

package engine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"shingoedge/domain"
	"shingoedge/store/processes"
)

// identityTagOf is the part-identity tag the monitor reads for a process: none
// when identity_none or no counter PLC is set, the override when typed, else
// CATID_0N derived from the counter tag.
func identityTagOf(p *processes.Process) string {
	if p == nil || p.IdentityNone || p.CounterPLCName == "" {
		return ""
	}
	if t := strings.TrimSpace(p.IdentityTagName); t != "" {
		return t
	}
	return deriveIdentityTag(p.CounterTagName)
}

// integerTagTypes are the WarLink tag types a counter may be: the poll reads
// every value as a JSON number and truncates it to an integer.
var integerTagTypes = map[string]bool{
	"SINT": true, "INT": true, "DINT": true, "LINT": true,
	"USINT": true, "UINT": true, "UDINT": true, "ULINT": true,
}

// EditProcessCounter writes a process's counter and part-identity
// configuration, re-points its reporting point, and re-primes the CATID monitor
// when the identity tag changed (the file comment has the rules). An edit that
// changes neither tag nor identity only writes the enabled flag.
func (e *Engine) EditProcessCounter(processID int64, edit domain.ProcessCounterEdit) error {
	edit.PLCName = strings.TrimSpace(edit.PLCName)
	edit.TagName = strings.TrimSpace(edit.TagName)
	edit.IdentityTagName = strings.TrimSpace(edit.IdentityTagName)

	e.cutoverMu.Lock()
	old, err := e.db.GetProcess(processID)
	if err != nil {
		e.cutoverMu.Unlock()
		return err
	}
	moved := old.CounterPLCName != edit.PLCName || old.CounterTagName != edit.TagName ||
		old.IdentityTagName != edit.IdentityTagName || old.IdentityNone != edit.IdentityNone
	if err := e.counterEditAllowed(old, edit, moved); err != nil {
		e.cutoverMu.Unlock()
		return err
	}
	if err := e.db.SetProcessCounter(processID, edit.PLCName, edit.TagName, edit.Enabled, edit.IdentityTagName, edit.IdentityNone); err != nil {
		e.cutoverMu.Unlock()
		return err
	}
	syncErr := e.syncPrimaryPoint(processID, old.CounterPLCName, old.CounterTagName)
	e.cutoverMu.Unlock()
	if syncErr != nil {
		return fmt.Errorf("re-point the reporting point: %w", syncErr)
	}

	cur, err := e.db.GetProcess(processID)
	if err != nil {
		return err
	}
	if cur.CounterPLCName == old.CounterPLCName && identityTagOf(cur) == identityTagOf(old) {
		return nil // the monitor's arm, debounce and verify watch stand
	}
	e.republishIdentity(old, cur)
	if e.catidMon != nil {
		e.catidMon.reprime(processID)
	}
	return nil
}

// counterEditAllowed refuses an edit that moves the counter or the identity
// while a changeover is open, a counter tag another process counts on, and a
// counter tag WarLink does not declare as an integer. Called under cutoverMu.
func (e *Engine) counterEditAllowed(old *processes.Process, edit domain.ProcessCounterEdit, moved bool) error {
	if !moved {
		return nil
	}
	if co, err := e.db.GetActiveProcessChangeover(old.ID); err == nil && co != nil {
		return fmt.Errorf("%w: a changeover to %s is open", ErrChangeoverActive, co.ToStyleName)
	}
	if edit.PLCName == "" || edit.TagName == "" {
		return nil
	}
	if owner, err := e.db.ProcessCounterOwner(edit.PLCName, edit.TagName, old.ID); err != nil {
		return err
	} else if owner != 0 {
		return fmt.Errorf("%s/%s is already the counter of another process (%s)", edit.PLCName, edit.TagName, e.processName(owner))
	}
	if edit.PLCName == old.CounterPLCName && edit.TagName == old.CounterTagName {
		return nil // the counter tag is the one already counting; only the identity moved
	}
	return e.checkCounterTagType(edit.PLCName, edit.TagName)
}

// checkCounterTagType checks a counter tag is an integer. A tag WarLink lists
// carries its declared type, and anything but an integer type is refused. A tag
// it does not list — a dotted member of a listed struct, which is how every
// production counter is named, since WarLink lists structs and not their
// members — is enabled and read, and accepted when the read is a whole number:
// the type is inferred from the read, not declared. A read that fails or is not
// whole is refused. (A REAL that happened to read whole is caught later by the
// observed clicks-per-count health line.) With no PLC manager (no WarLink at
// all) there is nothing to ask.
func (e *Engine) checkCounterTagType(plcName, tagName string) error {
	if e.plcMgr == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	tags, err := e.plcMgr.FetchAllTags(ctx, plcName)
	if err != nil {
		return fmt.Errorf("WarLink did not answer for PLC %s, so the counter tag's type cannot be checked: %w", plcName, err)
	}
	for _, t := range tags {
		if t.Name != tagName {
			continue
		}
		if integerTagTypes[strings.ToUpper(strings.TrimSpace(t.Type))] {
			return nil
		}
		return fmt.Errorf("%s is a %s tag in WarLink; a counter must be an integer tag (DINT)", tagName, t.Type)
	}
	if err := e.plcMgr.EnableTagPublishing(ctx, plcName, tagName); err != nil {
		return fmt.Errorf("WarLink does not list %s on PLC %s and would not publish it, so it cannot be read: %w", tagName, plcName, err)
	}
	v, err := e.plcMgr.ReadTagDirect(ctx, plcName, tagName)
	if err != nil {
		return fmt.Errorf("WarLink does not list %s on PLC %s and its read failed: %w", tagName, plcName, err)
	}
	if !wholeNumber(v) {
		return fmt.Errorf("%s on PLC %s reads %v; a counter must read a whole number", tagName, plcName, v)
	}
	log.Printf("counter tag %s/%s: not in WarLink's list; type inferred from the read (%v), not declared", plcName, tagName, v)
	return nil
}

// wholeNumber reports whether a tag read is an integer value: an integer type,
// or a float with no fractional part (every JSON number decodes as float64).
func wholeNumber(v any) bool {
	switch n := v.(type) {
	case int, int32, int64:
		return true
	case float32:
		return float64(n) == math.Trunc(float64(n))
	case float64:
		return !math.IsInf(n, 0) && !math.IsNaN(n) && n == math.Trunc(n)
	}
	return false
}

// republishIdentity moves WarLink's publishing from the process's old identity
// tag to its new one: the old is unpublished only when shingo enabled it, and
// the new is enabled, and marked shingo's, when WarLink is not already
// publishing it. Outside every lock: each call is an HTTP round trip.
func (e *Engine) republishIdentity(old, cur *processes.Process) {
	if e.plcMgr == nil {
		return
	}
	oldTag, newTag := identityTagOf(old), identityTagOf(cur)
	if old.IdentityWarlinkManaged && oldTag != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		if err := e.plcMgr.DisableTagPublishing(ctx, old.CounterPLCName, oldTag); err != nil {
			log.Printf("plc-catid: unpublish %s.%s for process %d: %v", old.CounterPLCName, oldTag, old.ID, err)
		}
		cancel()
	}
	managed := false
	if newTag != "" && !e.plcMgr.IsTagPublished(cur.CounterPLCName, newTag) {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		if err := e.plcMgr.EnableTagPublishing(ctx, cur.CounterPLCName, newTag); err != nil {
			log.Printf("plc-catid: publish %s.%s for process %d: %v", cur.CounterPLCName, newTag, cur.ID, err)
		} else {
			managed = true
		}
		cancel()
	}
	if err := e.db.SetProcessIdentityManaged(cur.ID, managed); err != nil {
		log.Printf("plc-catid: record identity publishing for process %d: %v", cur.ID, err)
	}
}

// syncPrimaryPoint brings the reporting point in line with the process's
// counter after a counter edit (oldPLC/oldTag are the names before it) or a
// style flip (the same names):
//
//   - blank counter names disable the point that read the old tag and
//     unpublish it if shingo published it — the process counts nothing;
//   - with no active style there is nothing to count under yet;
//   - a point already reading the counter tag is the process's: it is pointed
//     at the active style (adopting a deleted process's point on the same tag),
//     and the old tag's point, if another, is disabled;
//   - else the point that read the old tag moves to the new one (its first read
//     seeds); else a point is created.
//
// Found by tag, never by style: two points on one style (a ghost left by an
// older re-point) made a LIMIT 1 by style move the ghost
// (TestSync_TagEditNeverRepointsTheOrphan). A tag another process counts on is
// refused.
func (e *Engine) syncPrimaryPoint(processID int64, oldPLC, oldTag string) error {
	p, err := e.db.GetProcess(processID)
	if err != nil {
		return err
	}
	moved := oldPLC != p.CounterPLCName || oldTag != p.CounterTagName
	if p.CounterPLCName == "" || p.CounterTagName == "" {
		if oldPLC != "" && oldTag != "" {
			e.disablePoint(oldPLC, oldTag)
		}
		return nil
	}
	if p.ActiveStyleID == nil {
		return nil
	}
	style := *p.ActiveStyleID
	if owner, err := e.db.ProcessCounterOwner(p.CounterPLCName, p.CounterTagName, processID); err != nil {
		return err
	} else if owner != 0 {
		return fmt.Errorf("%s/%s is the counter of another process", p.CounterPLCName, p.CounterTagName)
	}
	if cur, err := e.db.GetReportingPointByTag(p.CounterPLCName, p.CounterTagName); err == nil && cur != nil {
		if cur.StyleID != style || cur.Enabled != p.CounterEnabled {
			if err := e.db.UpdateReportingPoint(cur.ID, cur.PLCName, cur.TagName, style, p.CounterEnabled); err != nil {
				return err
			}
		}
		if moved && oldPLC != "" && oldTag != "" {
			e.disablePoint(oldPLC, oldTag)
		}
		return nil
	}
	if moved && oldPLC != "" && oldTag != "" {
		if prev, err := e.db.GetReportingPointByTag(oldPLC, oldTag); err == nil && prev != nil {
			if err := e.db.UpdateReportingPoint(prev.ID, p.CounterPLCName, p.CounterTagName, style, p.CounterEnabled); err != nil {
				return err
			}
			e.ManageReportingPointTag(prev.ID, oldPLC, oldTag, prev.WarlinkManaged, p.CounterPLCName, p.CounterTagName)
			return nil
		}
	}
	rpID, err := e.db.CreateReportingPoint(p.CounterPLCName, p.CounterTagName, style)
	if err != nil {
		return err
	}
	if !p.CounterEnabled {
		if err := e.db.UpdateReportingPoint(rpID, p.CounterPLCName, p.CounterTagName, style, false); err != nil {
			e.logFn("warlink: update reporting point for process %d: %v", p.ID, err)
		}
	}
	e.EnsureTagPublished(rpID, p.CounterPLCName, p.CounterTagName)
	return nil
}

// disablePoint switches off the reporting point reading (plcName, tagName),
// if there is one, and unpublishes its tag when shingo published it.
func (e *Engine) disablePoint(plcName, tagName string) {
	rp, err := e.db.GetReportingPointByTag(plcName, tagName)
	if err != nil || rp == nil {
		return
	}
	if rp.Enabled {
		if err := e.db.UpdateReportingPoint(rp.ID, rp.PLCName, rp.TagName, rp.StyleID, false); err != nil {
			log.Printf("warlink: disable reporting point %d: %v", rp.ID, err)
			return
		}
	}
	if rp.WarlinkManaged && e.plcMgr != nil {
		e.EnsureTagUnpublished(rp.ID, rp.PLCName, rp.TagName)
		if err := e.db.SetReportingPointManaged(rp.ID, false); err != nil {
			log.Printf("warlink: clear RP %d managed: %v", rp.ID, err)
		}
	}
}

// unpublishProcessTags drops WarLink publishing shingo enabled for a process
// being deleted: its counter tag's and its identity tag's.
func (e *Engine) unpublishProcessTags(p *processes.Process) {
	if e.plcMgr == nil || p == nil {
		return
	}
	if p.CounterPLCName != "" && p.CounterTagName != "" {
		if rp, err := e.db.GetReportingPointByTag(p.CounterPLCName, p.CounterTagName); err == nil && rp != nil && rp.WarlinkManaged {
			e.EnsureTagUnpublished(rp.ID, rp.PLCName, rp.TagName)
		}
	}
	if tag := identityTagOf(p); tag != "" && p.IdentityWarlinkManaged {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if err := e.plcMgr.DisableTagPublishing(ctx, p.CounterPLCName, tag); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("plc-catid: unpublish %s.%s for deleted process %d: %v", p.CounterPLCName, tag, p.ID, err)
		}
	}
}
