package engine

import (
	"context"
	"sort"

	"shingocore/service"
)

// PayloadHealth is one row of the inventory Replenishment Health rollup: a
// payload's DB-truth on-hand (bins + lineside split), the threshold monitor's
// cached belief (for drift detection), and its configured threshold(s).
//
// OnHand is the DB truth (bin_uop + bucket_uop) — the same value the threshold
// monitor is supposed to hold. MonitorCachedTotal is the monitor's in-memory
// belief; when Monitored and the two disagree, the monitor has drifted and needs
// a re-baseline (an edge re-register or a bin reconcile). For unmonitored
// payloads MonitorCachedTotal mirrors OnHand (no belief to drift).
type PayloadHealth struct {
	PayloadCode        string           `json:"payload_code"`
	Description        string           `json:"description"`
	BinUOP             int              `json:"bin_uop"`
	BucketUOP          int              `json:"bucket_uop"`
	OnHand             int              `json:"on_hand"` // bin_uop + bucket_uop (DB truth)
	Monitored          bool             `json:"monitored"`
	MonitorCachedTotal int              `json:"monitor_cached_total"`
	Threshold          int              `json:"threshold"` // representative (max binding); 0 = unset
	Bindings           []MonitorBinding `json:"bindings"`
}

// ReplenishmentHealth builds the per-payload rollup behind the inventory
// Replenishment Health section. It unions the monitored payloads (those with a
// threshold binding) with every stocked payload, so unmonitored-but-stocked
// payloads still surface (as "no threshold set"). For each it reports the DB
// truth (bin + bucket UOP), the monitor's cached total (drift = cached vs
// on-hand), and the configured threshold(s). Rows come back sorted by payload
// code; the page applies its own worst-first ordering.
func (e *Engine) ReplenishmentHealth(ctx context.Context) ([]PayloadHealth, error) {
	var snap []MonitorSnapshotEntry
	if e.thresholdMonitor != nil {
		snap = e.thresholdMonitor.Snapshot()
	}
	byPayload := make(map[string]MonitorSnapshotEntry, len(snap))
	set := make(map[string]struct{}, len(snap))
	for _, s := range snap {
		byPayload[s.PayloadCode] = s
		set[s.PayloadCode] = struct{}{}
	}

	stocked, err := e.db.DistinctStockedPayloads()
	if err != nil {
		return nil, err
	}
	for _, p := range stocked {
		set[p] = struct{}{}
	}
	if len(set) == 0 {
		return []PayloadHealth{}, nil
	}

	payloads := make([]string, 0, len(set))
	for p := range set {
		payloads = append(payloads, p)
	}

	uop, err := e.inventoryService.SystemUOPForPayload(ctx, payloads)
	if err != nil {
		return nil, err
	}
	uopByCode := make(map[string]service.PayloadSystemUOP, len(uop.Counts))
	for _, c := range uop.Counts {
		uopByCode[c.PayloadCode] = c
	}

	descs, err := e.db.PayloadDescriptions()
	if err != nil {
		// Descriptions are cosmetic — degrade to codes only rather than fail the
		// whole page load.
		e.logFn("replenishment health: payload descriptions: %v", err)
		descs = map[string]string{}
	}

	out := make([]PayloadHealth, 0, len(payloads))
	for _, p := range payloads {
		u := uopByCode[p]
		row := PayloadHealth{
			PayloadCode: p,
			Description: descs[p],
			BinUOP:      u.BinUOP,
			BucketUOP:   u.BucketUOP,
			OnHand:      u.TotalUOP,
		}
		if s, ok := byPayload[p]; ok {
			row.Monitored = true
			row.MonitorCachedTotal = s.CachedTotal
			row.Bindings = s.Bindings
			for _, b := range s.Bindings {
				if b.Threshold > row.Threshold {
					row.Threshold = b.Threshold
				}
			}
		} else {
			// Unmonitored: no cached belief to drift — mirror the DB truth.
			row.MonitorCachedTotal = u.TotalUOP
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PayloadCode < out[j].PayloadCode })
	return out, nil
}
