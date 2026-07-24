package engine

import (
	"context"
	"sort"

	"shingocore/service"
)

// PayloadHealth is one row of the inventory Replenishment Health rollup: a
// payload's DB-truth on-hand (bins + lineside split), whether a threshold
// binding watches it, and its configured threshold(s).
//
// OnHand is the DB truth (bin_uop + bucket_uop) — and now the ONLY number in
// play. The monitor no longer keeps a private tally, so there is no "cached
// belief" that can drift from OnHand; the drift chip and its counter were
// deleted with that tally (the drift failure category no longer exists).
type PayloadHealth struct {
	PayloadCode string           `json:"payload_code"`
	Description string           `json:"description"`
	BinUOP      int              `json:"bin_uop"`
	BucketUOP   int              `json:"bucket_uop"`
	OnHand      int              `json:"on_hand"` // bin_uop + bucket_uop (DB truth)
	Monitored   bool             `json:"monitored"`
	Threshold   int              `json:"threshold"` // representative (max binding); 0 = unset
	Bindings    []MonitorBinding `json:"bindings"`
	// SwapContradiction is true when a manual swap was requested for this
	// payload while the ledger read it as fully stocked (P2-C9) — surfaced as a
	// right-hand chip on the Replenishment Health row.
	SwapContradiction bool `json:"swap_contradiction"`
}

// ReplenishmentHealth builds the per-payload rollup behind the inventory
// Replenishment Health section. It unions the monitored payloads (those with a
// threshold binding) with every stocked payload, so unmonitored-but-stocked
// payloads still surface (as "no threshold set"). For each it reports the DB
// truth (bin + bucket UOP) and the configured threshold(s). Rows come back
// sorted by payload code; the page applies its own worst-first ordering.
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
			row.Bindings = s.Bindings
			row.SwapContradiction = s.SwapContradiction
			for _, b := range s.Bindings {
				if b.Threshold > row.Threshold {
					row.Threshold = b.Threshold
				}
			}
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PayloadCode < out[j].PayloadCode })
	return out, nil
}
