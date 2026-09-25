package service

import (
	"fmt"
	"log"
	"strconv"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingocore/store"
)

// lineside_divergence.go — the lineside report is a per-carrier checksum.
//
// THE RULING (seat-count round 1 §5, 2026-09-23; round 2 S8, 2026-09-24).
// There is one count per carrier. The Edge counts consumption and sends it as
// sequenced deltas and pile levels; Core's replica (bins.uop_remaining,
// lineside_buckets) is what every replenishment decision reads. The Edge's 60s lineside report
// decides nothing: Core compares it against the replica on ingest and records
// each disagreement as a report_divergence episode in bin_uop_exception,
// opened when first seen and recovered when a later report no longer shows it.
// Nothing heals from it. A divergence is corrected through the front door (a
// count correction, which already binds a staged bin), and a cause that recurs
// is fixed on the delta path. A second writer from the report would be a
// reconciler, which round 4 ruled out.
//
// THE SIX CLASSES, per report row or per Core carrier:
//
//   - count: the Edge's bound carrier and Core's bin agree on the seat and the
//     generation, the counts differ, and Core has applied exactly the deltas
//     the report's count is stated at (Core's last_seq == the row's
//     FlushedSeq; the Edge states the count as of that seq). last_seq below it
//     is a delta in flight; above it, a later window reached Core ahead of the
//     report (the Edge enqueues the report behind every delta its count
//     includes, so only a transport reorder or an Edge restore does this).
//     Either way the count is undecided: an open count episode is neither
//     opened nor closed on it. Round 2 S8 wrote ">="; equality is the exact
//     form once the report states its count as of FlushedSeq.
//   - epoch: the Edge counts the carrier under a generation other than Core's.
//     Core drops every delta of a retired generation, so the counts cannot
//     converge. No seq test applies: the dropped deltas never advance last_seq,
//     so gating on it would silence exactly this case. Epoch 0 on the Edge is
//     the unknown sentinel that Core always applies, and is not a divergence.
//   - not_at_seat: the Edge's bound carrier is one Core places at another node,
//     or does not know.
//   - empty_seat: a counted Core carrier (a payload, a non-zero count, a live
//     status) sits at a seat this report names with nothing bound — the SNF3
//     shape, Core reading 150 where the Edge has no carrier. Decided from the
//     report's own rows, with no station-to-seat map: since the owner's
//     2026-09-24 ruling the Edge sends every seat it runs, an empty one as a
//     row with nothing bound.
//   - unbound_carrier: a counted Core carrier sits at one of the station's
//     consume seats that the report does not show as empty, and no row binds
//     it: a second carrier at a seat where the Edge has bound another, or a
//     seat the report leaves out. The station's seats are the nodes its report
//     names, and the nodes it has reported before (edge_lineside_reports) that
//     an active style claims as consume nodes in the plant-claims mirror.
//   - bucket: the Edge's active pile for the seat's part differs from Core's
//     active mirror row for that seat and part. The report carries no pile
//     seq, so there is no in-flight test. The Edge states the pile as of its
//     flushed levels and enqueues the report behind every one of them; the
//     per-station partition delivers them in that order, so Core has applied
//     them by the time it compares. Only a transport reorder breaks that.
//
// WHAT IS NOT COMPARED. A row from an Edge that predates the carrier keys
// (BinCount 1, BinID nil) says a carrier is bound and not which: no carrier
// class runs for it, and Core carriers at that seat are not called unbound.
// Its bucket figure is still compared. A row with no part (an empty seat, or a
// carrier known to be empty) has no bucket to compare. A station that sends no
// report compares nothing, so its open episodes stay open until it reports
// again; the Edge sends one every interval, with no rows when it runs no
// consume seat, and that closes them.
//
// Episode opens and closes are logged, one line each, so the journal carries
// the same record as the page.

// The report_divergence classes, stored in bin_uop_exception.op. Stable
// strings: episodes reference them.
const (
	ReportDivergenceCount          = "count"
	ReportDivergenceEpoch          = "epoch"
	ReportDivergenceNotAtSeat      = "not_at_seat"
	ReportDivergenceUnboundCarrier = "unbound_carrier"
	ReportDivergenceEmptySeat      = "empty_seat"
	ReportDivergenceBucket         = "bucket"
)

// LinesideDivergenceStore is the store surface the comparison needs.
type LinesideDivergenceStore interface {
	LinesideCoreSide(station string, carriers []store.LinesideReportCarrier, seats []store.LinesideReportSeat) (store.LinesideCoreSide, error)
	OpenReportDivergenceKeys(station string) (map[string]int64, error)
	RecordReportDivergences(station string, open []store.ReportDivergence, closed []int64, at time.Time) error
}

var _ LinesideDivergenceStore = (*store.DB)(nil)

// LinesideDivergenceService compares one station's lineside report against
// Core's replica and records the episodes.
type LinesideDivergenceService struct {
	db LinesideDivergenceStore
}

// NewLinesideDivergenceService builds the comparison over db.
func NewLinesideDivergenceService(db LinesideDivergenceStore) *LinesideDivergenceService {
	return &LinesideDivergenceService{db: db}
}

// CheckReport compares one report's entries against Core's replica and opens
// and closes report_divergence episodes for the station. Two reads, whatever
// the row count, and a write only on a transition.
func (s *LinesideDivergenceService) CheckReport(station string, entries []protocol.LinesideLevelEntry) (opened, closed int, err error) {
	var carriers []store.LinesideReportCarrier
	var seats []store.LinesideReportSeat
	for _, e := range entries {
		if e.CoreNodeName == "" {
			continue
		}
		seats = append(seats, store.LinesideReportSeat{Node: e.CoreNodeName, Payload: e.PayloadCode})
		if e.BinCount > 0 && e.BinID != nil {
			carriers = append(carriers, store.LinesideReportCarrier{BinID: *e.BinID, Epoch: e.BinEpoch})
		}
	}
	core, err := s.db.LinesideCoreSide(station, carriers, seats)
	if err != nil {
		return 0, 0, err
	}
	divs, undecided := ClassifyLinesideReport(entries, core)
	openKeys, err := s.db.OpenReportDivergenceKeys(station)
	if err != nil {
		return 0, 0, err
	}
	seen := make(map[string]bool, len(divs))
	var toOpen []store.ReportDivergence
	for _, d := range divs {
		seen[d.Key] = true
		if _, ok := openKeys[d.Key]; !ok {
			toOpen = append(toOpen, d)
		}
	}
	var toClose []int64
	var closedKeys []string
	for key, id := range openKeys {
		if !seen[key] && !undecided[key] {
			toClose = append(toClose, id)
			closedKeys = append(closedKeys, key)
		}
	}
	if err := s.db.RecordReportDivergences(station, toOpen, toClose, clock.Now()); err != nil {
		return 0, 0, err
	}
	for _, d := range toOpen {
		log.Printf("lineside report DIVERGENCE OPENED station=%s class=%s node=%s payload=%s bin=%s edge=%s core=%s",
			station, d.Class, d.Node, d.Payload, ptrStr(d.BinID), ptrStr(d.EdgeCount), ptrStr(d.CoreCount))
	}
	for _, key := range closedKeys {
		log.Printf("lineside report divergence recovered station=%s key=%s", station, key)
	}
	return len(toOpen), len(toClose), nil
}

// ClassifyLinesideReport is the comparison, pure: the divergences one report
// shows against Core's side, and the keys of count episodes it cannot decide
// because deltas are still in flight (an open one is neither closed nor
// re-opened on them).
func ClassifyLinesideReport(entries []protocol.LinesideLevelEntry, core store.LinesideCoreSide) (divs []store.ReportDivergence, undecided map[string]bool) {
	undecided = map[string]bool{}
	byID := make(map[int64]store.LinesideCoreCarrier, len(core.Carriers))
	for _, c := range core.Carriers {
		byID[c.BinID] = c
	}
	bound := map[int64]bool{}
	unidentifiedSeat := map[string]bool{}
	// A seat is empty when the report names it and no row there has anything
	// bound; a seat with any bound row is occupied.
	emptySeat := map[string]bool{}
	occupiedSeat := map[string]bool{}
	seen := map[string]bool{}
	add := func(d store.ReportDivergence) {
		if !seen[d.Key] {
			seen[d.Key] = true
			divs = append(divs, d)
		}
	}
	for _, e := range entries {
		if e.CoreNodeName == "" {
			continue
		}
		if e.BinCount == 0 && e.BinID == nil {
			emptySeat[e.CoreNodeName] = true
		} else {
			occupiedSeat[e.CoreNodeName] = true
		}
		if e.BinCount > 0 && e.BinID == nil {
			unidentifiedSeat[e.CoreNodeName] = true
		}
		if e.BinCount > 0 && e.BinID != nil {
			bound[*e.BinID] = true
			d, inFlight := classifyCarrier(e, byID)
			if d != nil {
				add(*d)
			}
			if inFlight != "" {
				undecided[inFlight] = true
			}
		}
		// A row with no part has no bucket to compare.
		if e.PayloadCode == "" {
			continue
		}
		coreQty := core.Buckets[store.LinesideReportSeat{Node: e.CoreNodeName, Payload: e.PayloadCode}]
		if e.BucketQty != coreQty {
			edge, c := e.BucketQty, coreQty
			add(store.ReportDivergence{
				Key:   divergenceKey(ReportDivergenceBucket, e.CoreNodeName, e.PayloadCode, nil),
				Class: ReportDivergenceBucket, Node: e.CoreNodeName, Payload: e.PayloadCode,
				EdgeCount: &edge, CoreCount: &c,
			})
		}
	}
	for _, c := range core.Carriers {
		if !c.AtSeat || bound[c.BinID] || unidentifiedSeat[c.NodeName] {
			continue
		}
		class := ReportDivergenceUnboundCarrier
		if emptySeat[c.NodeName] && !occupiedSeat[c.NodeName] {
			class = ReportDivergenceEmptySeat
		}
		id, uop, epoch := c.BinID, c.UOP, c.Epoch
		add(store.ReportDivergence{
			Key:   divergenceKey(class, c.NodeName, c.Payload, &id),
			Class: class, BinID: &id, Node: c.NodeName, Payload: c.Payload,
			CoreCount: &uop, CoreEpoch: &epoch,
		})
	}
	return divs, undecided
}

// classifyCarrier compares one bound row against Core's bin. It returns the
// divergence, if any, and the count key when the gap is a delta in flight.
func classifyCarrier(e protocol.LinesideLevelEntry, byID map[int64]store.LinesideCoreCarrier) (*store.ReportDivergence, string) {
	id := *e.BinID
	edgeCount, edgeEpoch := e.BinUOP, e.BinEpoch
	d := store.ReportDivergence{
		BinID: &id, Node: e.CoreNodeName, Payload: e.PayloadCode,
		EdgeCount: &edgeCount, EdgeEpoch: &edgeEpoch,
	}
	c, ok := byID[id]
	if ok {
		coreCount, coreEpoch := c.UOP, c.Epoch
		d.CoreCount, d.CoreEpoch = &coreCount, &coreEpoch
	}
	switch {
	case !ok || c.NodeName != e.CoreNodeName:
		d.Class = ReportDivergenceNotAtSeat
	case e.BinEpoch > 0 && e.BinEpoch != c.Epoch:
		d.Class = ReportDivergenceEpoch
	case e.BinUOP != c.UOP:
		key := divergenceKey(ReportDivergenceCount, e.CoreNodeName, e.PayloadCode, &id)
		if c.LastSeq != e.FlushedSeq {
			return nil, key
		}
		flushed, last := e.FlushedSeq, c.LastSeq
		d.Class, d.FlushedSeq, d.CoreLastSeq = ReportDivergenceCount, &flushed, &last
	default:
		return nil, ""
	}
	d.Key = divergenceKey(d.Class, d.Node, d.Payload, &id)
	return &d, ""
}

// divergenceKey names one episode within its station.
func divergenceKey(class, node, payload string, bin *int64) string {
	b := "-"
	if bin != nil {
		b = strconv.FormatInt(*bin, 10)
	}
	return class + "|" + node + "|" + payload + "|" + b
}

func ptrStr[T int | int64](p *T) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprint(*p)
}
