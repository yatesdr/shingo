package rds

import (
	"errors"
	"time"

	"shingocore/fleet"
)

// UnreadableAfter is how long one order's reads must keep failing, while other
// orders read fine, before it is reported. Long enough that a blip or an RDS
// restart's first seconds says nothing; short against a press waiting on a
// robot shingo cannot see.
const UnreadableAfter = 5 * time.Minute

// UnreadableEmitter is the OPTIONAL half of PollerEmitter: an order RDS cannot
// report on. Optional so the emitters that do not care (test fakes) need not
// grow it.
//
// WHY IT EXISTS. A failed GetOrderDetails used to be a log line and a
// `continue`: the order kept its last status — "in transit" on every screen —
// for as long as RDS failed to answer about it, and in_transit is exempt from
// the stuck sweep. RDS answers "order details is empty" for an id it does not
// hold (HK, 2026-11-04). Nothing told anyone.
type UnreadableEmitter interface {
	EmitOrderUnreadable(orderID int64, rdsOrderID string, since time.Time, reason string)
	EmitOrderReadable(orderID int64, rdsOrderID string)
}

// unreadableThreshold is UnreadableAfter unless a test set the poller's own.
func (p *Poller) unreadableThreshold() time.Duration {
	if p.unreadableAfter > 0 {
		return p.unreadableAfter
	}
	return UnreadableAfter
}

type unreadableOrder struct {
	since    time.Time
	reason   string
	reported bool
}

// noteUnreadable records a failed read of one order. The first failure starts
// the clock; later ones keep the latest reason.
func (p *Poller) noteUnreadable(rdsID string, err error) {
	reason := err.Error()
	var ae *APIError
	if errors.As(err, &ae) {
		reason = fleet.FormatVendorError(ae.VendorCode(), ae.VendorText())
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.unreadable == nil {
		p.unreadable = map[string]*unreadableOrder{}
	}
	if u, ok := p.unreadable[rdsID]; ok {
		u.reason = reason
		return
	}
	p.unreadable[rdsID] = &unreadableOrder{since: time.Now(), reason: reason}
}

// noteReadable clears an order whose read succeeded, and says so if it had
// been reported.
func (p *Poller) noteReadable(rdsID string) {
	p.mu.Lock()
	u, ok := p.unreadable[rdsID]
	delete(p.unreadable, rdsID)
	p.mu.Unlock()
	if !ok || !u.reported {
		return
	}
	ue, isUE := p.emitter.(UnreadableEmitter)
	if !isUE {
		return
	}
	if oid, err := p.resolver.ResolveRDSOrderID(rdsID); err == nil {
		ue.EmitOrderReadable(oid, rdsID)
	}
}

// reportUnreadable reports, once each, the orders that have been unreadable for
// UnreadableAfter — but only on a pass where OTHER orders read fine. A pass
// where every read failed is RDS down or unreachable: one fleet-wide condition
// that the connection health check reports, not one alert per order.
func (p *Poller) reportUnreadable(failed, total int) {
	if failed == 0 || failed == total {
		return
	}
	ue, isUE := p.emitter.(UnreadableEmitter)
	if !isUE {
		return
	}
	now := time.Now()
	type due struct {
		rdsID  string
		since  time.Time
		reason string
	}
	var report []due
	p.mu.Lock()
	for id, u := range p.unreadable {
		if !u.reported && now.Sub(u.since) >= p.unreadableThreshold() {
			u.reported = true
			report = append(report, due{id, u.since, u.reason})
		}
	}
	p.mu.Unlock()
	for _, d := range report {
		oid, err := p.resolver.ResolveRDSOrderID(d.rdsID)
		if err != nil {
			continue
		}
		ue.EmitOrderUnreadable(oid, d.rdsID, d.since, d.reason)
	}
}
