package engine

import (
	"fmt"

	"shingo/protocol"
	"shingocore/store/orders"
	"shingocore/store/reconciliation"
)

// ── AN ASK A CRASH LEFT OPEN ───────────────────────────────────────────────
//
// The hand opens an order's ask (orders.asked_vendor_order_id) before it asks
// the fleet for the job, and the fleet's answer closes it. A Core that dies
// inside the create leaves the ask on the row: the one record of a job the
// fleet may or may not hold. Nothing asks for it again (an open ask is not
// acquiring), and nothing releases its holds, because Core cannot tell which it
// is: the fleet may be running it.
//
// So at boot, before anything in this process can open an ask, every open ask
// is one a previous Core left (one Core per database). TakeBootAsks names each:
//
//   - on a live row it is logged, and listed as an anomaly naming the job for as
//     long as the row holds that ask. The order keeps its status and its holds,
//     and the stuck flag ages it: it writes no history while it waits;
//   - on an ended row (the order ended during the create, then Core died) the
//     job goes to the terminate chase (orders.ChaseEndedAsk), which asks the
//     fleet about it and re-sends the terminate. A terminal row carries no ask.
//
// A listed order a person then ends goes to the chase on the next
// reconciliation pass, for the same reason.
//
// Resolving a live one from the fleet's answer is not built here: it needs what
// the fleet answers for an id it does not hold, and it is the next brief's.

// TakeBootAsks names every ask open at boot. Engine.Start calls it first,
// before anything in this process can open one.
func (s *ReconciliationService) TakeBootAsks() {
	asks, err := s.db.ListOpenAsks()
	if err != nil {
		s.logFn("engine: boot: list open asks: %v (an ask a crash left open is not named this boot)", err)
		return
	}
	live := map[int64]string{}
	for _, a := range asks {
		if protocol.IsTerminal(a.Status) {
			s.chaseEndedAsk(a, "at boot")
			continue
		}
		s.logFn("engine: BOOT ANOMALY: order %d (%s) holds ask %s from before this boot: Core asked the fleet "+
			"for that job and stopped before the answer. The order keeps its holds and is not asked again "+
			"until a person ends it; check the fleet for %s", a.OrderID, a.Status, a.Asked, a.Asked)
		live[a.OrderID] = a.Asked
	}
	s.bootMu.Lock()
	s.bootAsks = live
	s.bootMu.Unlock()
}

// chaseEndedAsk hands an ended row's open ask to the terminate chase. Reports
// whether the row was written.
func (s *ReconciliationService) chaseEndedAsk(a orders.OpenAsk, when string) bool {
	ok, err := s.db.ChaseEndedOrderAsk(a.OrderID, a.Asked)
	if err != nil {
		s.logFn("engine: order %d is %s with ask %s open (%s), and its job could not be named for the "+
			"terminate chase: %v (check the fleet for %s)", a.OrderID, a.Status, a.Asked, when, err, a.Asked)
		return false
	}
	if ok {
		s.logFn("engine: order %d is %s with ask %s open (%s): the job goes to the terminate chase, which "+
			"asks the fleet about it and stops it if it runs", a.OrderID, a.Status, a.Asked, when)
	}
	return ok
}

// openBootAsks is each ask named at boot that its row still holds, as the row
// reads now.
func (s *ReconciliationService) openBootAsks() ([]orders.OpenAsk, error) {
	s.bootMu.Lock()
	n := len(s.bootAsks)
	s.bootMu.Unlock()
	if n == 0 {
		return nil, nil
	}
	all, err := s.db.ListOpenAsks()
	if err != nil {
		return nil, err
	}
	s.bootMu.Lock()
	defer s.bootMu.Unlock()
	var out []orders.OpenAsk
	for _, a := range all {
		if asked, ok := s.bootAsks[a.OrderID]; ok && asked == a.Asked {
			out = append(out, a)
		}
	}
	return out, nil
}

// chaseEndedBootAsks hands to the terminate chase each ask named at boot whose
// row a person has since ended, and forgets the ones that closed.
func (s *ReconciliationService) chaseEndedBootAsks() {
	open, err := s.openBootAsks()
	if err != nil {
		s.logFn("engine: boot asks: list open asks: %v", err)
		return
	}
	still := map[int64]string{}
	for _, a := range open {
		if protocol.IsTerminal(a.Status) && s.chaseEndedAsk(a, "ended after boot") {
			continue
		}
		still[a.OrderID] = a.Asked
	}
	s.bootMu.Lock()
	s.bootAsks = still
	s.bootMu.Unlock()
}

// bootAskAnomalies lists each ask named at boot that its row still holds.
func (s *ReconciliationService) bootAskAnomalies() ([]*reconciliation.Anomaly, error) {
	open, err := s.openBootAsks()
	if err != nil {
		return nil, err
	}
	out := make([]*reconciliation.Anomaly, 0, len(open))
	for _, a := range open {
		id := a.OrderID
		out = append(out, &reconciliation.Anomaly{
			Category:    "order_ask",
			Severity:    "critical",
			Issue:       "ask_open_at_boot",
			OrderID:     &id,
			StationID:   a.StationID,
			OrderStatus: string(a.Status),
			Detail: fmt.Sprintf("Core asked the fleet for job %s and stopped before the answer, so it does not "+
				"know whether the fleet has it. The order keeps its holds and is not asked again. Check the "+
				"fleet manager for %s, then cancel this order: its holds are released, and the terminate "+
				"chase stops the job if the fleet is running it", a.Asked, a.Asked),
		})
	}
	return out, nil
}
