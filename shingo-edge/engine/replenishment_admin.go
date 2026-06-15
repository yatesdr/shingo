// replenishment_admin.go — engine wrappers for the UOP-threshold
// replenishment admin page (handlers_admin_replenishment.go +
// handlers_api_replenishment.go).
//
// Engine-level wrappers exist so the www layer's narrow
// ServiceAccess / EngineOrchestration interface doesn't leak the wide
// *store.DB surface. Methods here trigger ClaimSync on writes — the
// new threshold value isn't visible to Core's threshold monitor until
// the registry sync lands, and we want that visible immediately rather
// than waiting for the next heartbeat-driven sync cycle.

package engine

import (
	"database/sql"
	"fmt"
	"time"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/service"
	"shingoedge/store"
	"shingoedge/store/orders"
	"shingoedge/store/processes"
)

// processClaimToInput maps a persisted NodeClaim back to its
// NodeClaimInput shape — needed because the admin replenishment edits
// touch only a subset of claim fields but UpsertStyleNodeClaim writes
// the whole row.
func processClaimToInput(c *processes.NodeClaim) domain.NodeClaimInput {
	return domain.NodeClaimInput{
		StyleID:               c.StyleID,
		CoreNodeName:          c.CoreNodeName,
		Role:                  c.Role,
		SwapMode:              c.SwapMode,
		PayloadCode:           c.PayloadCode,
		UOPCapacity:           c.UOPCapacity,
		ReorderPoint:          c.ReorderPoint,
		ReorderPointSource:    c.ReorderPointSource,
		AutoReorder:           c.AutoReorder,
		InboundStaging:        c.InboundStaging,
		OutboundStaging:       c.OutboundStaging,
		InboundSource:         c.InboundSource,
		OutboundDestination:   c.OutboundDestination,
		AllowedPayloadCodes:   c.AllowedPayloadCodes,
		AutoRequestPayload:    c.AutoRequestPayload,
		KeepStaged:            c.KeepStaged,
		EvacuateOnChangeover:  c.EvacuateOnChangeover,
		PairedCoreNode:        c.PairedCoreNode,
		SecondPairedCoreNode:  c.SecondPairedCoreNode,
		AutoConfirm:           c.AutoConfirm,
		Sequence:              c.Sequence,
		LinesideSoftThreshold: c.LinesideSoftThreshold,
		ReuseCompatibleBins:   c.ReuseCompatibleBins,
		AutoPush:              c.AutoPush,
	}
}

// LoaderThresholdRow is the engine-facing read shape for one row of
// loader_payload_thresholds. v6: keyed by core_node_name (canonical
// cross-system identifier). OverriddenInputs is a comma-separated
// list of input field names the engineer overrode on the last
// Calculate that produced the current threshold. BinCapacityUOP is the
// matching loader claim's UOPCapacity resolved at read time so the
// admin UI can render the "≈ N bins" annotation next to the threshold;
// 0 means no claim was resolvable (e.g. legacy row, payload no longer
// claimed) and the annotation is suppressed.
type LoaderThresholdRow struct {
	CoreNodeName          string  `json:"core_node_name"`
	PayloadCode           string  `json:"payload_code"`
	ReplenishUOPThreshold int     `json:"replenish_uop_threshold"`
	Source                string  `json:"source"`
	SafetyFactor          float64 `json:"safety_factor"`
	LookbackDays          int     `json:"lookback_days"`
	ThresholdConfidence   string  `json:"threshold_confidence,omitempty"`
	OverriddenInputs      string  `json:"overridden_inputs,omitempty"`
	BinCapacityUOP        int     `json:"bin_capacity_uop"`
	CycleSeconds          float64 `json:"cycle_seconds"` // per-part, from payload_catalog
}

// ListLoaderThresholds returns one row per (loader, payload) binding,
// merged with any saved loader_payload_thresholds row, sorted by
// (core_node_name, payload_code).
//
// Returning the binding inventory (not just saved rows) is what makes
// the replenishment page actionable: an engineer on a fresh plant has
// somewhere to type a threshold or cycle before any row exists. A
// binding with no saved row reports threshold=0 / source="legacy" /
// blank confidence — Apply on such a row creates the underlying
// loader_payload_thresholds row.
//
// Each row carries BinCapacityUOP (from any loader claim binding the
// payload — for the implied-bins annotation) and CycleSeconds (from
// payload_catalog — the per-part value the calculator uses).
//
// Bindings include every claim with role=produce + swap_mode=manual_swap
// across all styles on all processes — same shape as the cell-autoreorder
// listing on the same page, so engineers can configure replenishment for
// styles that aren't currently running.
func (e *Engine) ListLoaderThresholds() ([]LoaderThresholdRow, error) {
	pairs, err := e.listAllLoaderBindings()
	if err != nil {
		return nil, err
	}
	savedRows, err := e.db.ListLoaderPayloadThresholds()
	if err != nil {
		return nil, err
	}
	type key struct{ node, payload string }
	savedByKey := make(map[key]int, len(savedRows))
	for i, r := range savedRows {
		savedByKey[key{r.CoreNodeName, r.PayloadCode}] = i
	}
	out := make([]LoaderThresholdRow, 0, len(pairs))
	for _, p := range pairs {
		row := LoaderThresholdRow{
			CoreNodeName:   p.CoreNodeName,
			PayloadCode:    p.PayloadCode,
			BinCapacityUOP: p.UOPCapacity,
			Source:         "legacy",
		}
		if idx, ok := savedByKey[key{p.CoreNodeName, p.PayloadCode}]; ok {
			s := savedRows[idx]
			row.ReplenishUOPThreshold = s.ReplenishUOPThreshold
			row.Source = s.Source
			row.SafetyFactor = s.SafetyFactor
			row.LookbackDays = s.LookbackDays
			row.ThresholdConfidence = s.ThresholdConfidence
			row.OverriddenInputs = s.OverriddenInputs
		}
		if entry, err := e.db.GetPayloadCatalogByCode(p.PayloadCode); err == nil && entry != nil {
			row.CycleSeconds = entry.CycleSeconds
		}
		out = append(out, row)
	}
	return out, nil
}

// listAllLoaderBindings is the all-style version of
// ListLoaderClaimsForRecalculate — every (loader, payload) pair across
// every style on every process, deduplicated. Used by the page render
// and the recalc-all sweep; both want the full configurable surface,
// not just bindings on currently-active styles.
func (e *Engine) listAllLoaderBindings() ([]LoaderClaimPair, error) {
	seen := map[string]bool{}
	var out []LoaderClaimPair
	err := processes.WalkClaims(e.db.DB, processes.WalkOpts{
		Role:     protocol.ClaimRoleProduce,
		SwapMode: protocol.SwapModeManualSwap,
	}, func(ctx processes.WalkCtx) bool {
		for _, payload := range ctx.Claim.AllowedPayloads() {
			k := ctx.Claim.CoreNodeName + "|" + payload
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, LoaderClaimPair{
				CoreNodeName: ctx.Claim.CoreNodeName,
				PayloadCode:  payload,
				UOPCapacity:  ctx.Claim.UOPCapacity,
			})
		}
		return false
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// LoaderThresholdInput is the write shape from the API handler.
// ThresholdCalculated + ThresholdCalculatedAt + Confidence are stamped
// by the Apply / Override paths from the audit row so the UI badge
// reflects "this value came from a calculate run on date X with
// confidence C" without a follow-up read. OverriddenInputs is the
// comma-separated list of calculator-input field names the engineer
// overrode on the Calculate that produced this threshold.
type LoaderThresholdInput struct {
	CoreNodeName          string
	PayloadCode           string
	ReplenishUOPThreshold int
	Source                string
	SafetyFactor          float64
	Confidence            string
	ThresholdCalculated   int
	ThresholdCalculatedAt sql.NullString
	OverriddenInputs      string
	UpdatedBy             string
}

// UpsertLoaderThreshold writes a loader_payload_thresholds row and
// re-issues ClaimSync so Core's demand_registry threshold + monitor
// debounce reflect the new value immediately. Source defaults to
// 'manual' when blank; the API handler can override to 'calculated'
// after running the calculator.
func (e *Engine) UpsertLoaderThreshold(in LoaderThresholdInput) error {
	if in.CoreNodeName == "" || in.PayloadCode == "" {
		return fmt.Errorf("loader threshold: core_node_name and payload_code required")
	}
	if in.ReplenishUOPThreshold < 0 {
		return fmt.Errorf("loader threshold: replenish_uop_threshold must be >= 0")
	}
	t := loaderInputToRow(in)
	if err := e.db.UpsertLoaderPayloadThreshold(t); err != nil {
		return fmt.Errorf("upsert loader threshold: %w", err)
	}
	// Sync to Core so the new threshold engages immediately — Core's
	// SyncRegistry returns the change set and resets the per-binding
	// debounce timer, so a near-threshold event lands cleanly.
	e.SendClaimSync()
	return nil
}

// loaderInputToRow maps the engine-facing input shape onto the
// store-level row. Source defaults to 'manual' when blank.
func loaderInputToRow(in LoaderThresholdInput) store.LoaderPayloadThreshold {
	source := in.Source
	if source == "" {
		source = "manual"
	}
	return store.LoaderPayloadThreshold{
		CoreNodeName:          in.CoreNodeName,
		PayloadCode:           in.PayloadCode,
		ReplenishUOPThreshold: in.ReplenishUOPThreshold,
		Source:                source,
		SafetyFactor:          in.SafetyFactor,
		LookbackDays:          14,
		ThresholdCalculated:   in.ThresholdCalculated,
		ThresholdCalculatedAt: in.ThresholdCalculatedAt,
		ThresholdConfidence:   in.Confidence,
		OverriddenInputs:      in.OverriddenInputs,
		UpdatedBy:             in.UpdatedBy,
	}
}

// DeleteLoaderThreshold removes a row. Equivalent semantically to a
// threshold = 0 row — Edge falls back to bin-count for the pair. The
// API handler offers both paths so engineers can distinguish "remove
// the configuration row" from "considered, opted out" in the source
// audit.
func (e *Engine) DeleteLoaderThreshold(coreNodeName, payloadCode string) error {
	if err := e.db.DeleteLoaderPayloadThreshold(coreNodeName, payloadCode); err != nil {
		return err
	}
	e.SendClaimSync()
	return nil
}

// CalculateInput is the engineer's choice on the modal: payload + date
// range + safety factor + cycle time. CoreNodeName identifies the
// loader binding the result applies to.
type CalculateInput struct {
	CoreNodeName   string
	PayloadCode    string
	DateRangeStart time.Time
	DateRangeEnd   time.Time
	SafetyFactor   float64
	CycleSeconds   float64
}

// CalculateThresholdForLoader runs the engineer-triggered calculator
// against the given binding + date range. Returns the proposed
// result for UI display; the engineer then chooses Apply / Override
// / Cancel via separate calls.
//
// Bin capacity is a property of the PAYLOAD, so it resolves from the payload
// catalog — the same source HandleLoopBelowThreshold uses (catalogService.GetByCode).
// Catalog lookup is not style-gated, so a calculation against a payload on an
// inactive style (commissioning, calibration, multi-process plants) still finds its
// UOPCapacity. The capacity is carried on the response Inputs so the UI can render
// the implied-bin annotation next to the threshold; the calculator itself does not
// consume it. (Pre-cutover this read the capacity off the manual_swap shim claim,
// where it was synthesized as 0 — a silent UI-annotation gap; sourcing from the
// catalog both removes the last shim caller and makes the annotation non-zero.)
func (e *Engine) CalculateThresholdForLoader(in CalculateInput) (service.CalculateResult, error) {
	cap := 0
	if entry, err := e.catalogService.GetByCode(in.PayloadCode); err == nil && entry != nil {
		cap = entry.UOPCapacity
	}
	svc := service.NewThresholdCalculatorService(e.db)
	return svc.Calculate(service.CalculateRequest{
		CoreNodeName:   in.CoreNodeName,
		PayloadCode:    in.PayloadCode,
		DateRange:      orders.DateRange{Start: in.DateRangeStart, End: in.DateRangeEnd},
		SafetyFactor:   in.SafetyFactor,
		BinCapacityUOP: cap,
		CycleSeconds:   in.CycleSeconds,
	})
}

// ApplyCalculatedThreshold writes the calculated threshold value to
// loader_payload_thresholds with source='calculated'. The
// threshold_calculated / threshold_calculated_at / threshold_confidence
// columns are stamped from the LoaderThresholdInput so the UI badge
// reflects the run that produced the value. The Calculate response
// the client echoes back is the source of truth for those columns —
// there is no separate audit-table lookup.
func (e *Engine) ApplyCalculatedThreshold(in LoaderThresholdInput) error {
	if in.CoreNodeName == "" || in.PayloadCode == "" {
		return fmt.Errorf("apply calculated threshold: core_node_name and payload_code required")
	}
	in.Source = "calculated"
	return e.UpsertLoaderThreshold(in)
}

// OverrideCalculatedThreshold writes the engineer's manual value to
// loader_payload_thresholds with source='manual'. threshold_calculated
// still records what the calculator suggested so "calculator said X,
// engineer chose Y" is recoverable from the threshold row.
func (e *Engine) OverrideCalculatedThreshold(overrideValue int, in LoaderThresholdInput) error {
	if in.CoreNodeName == "" || in.PayloadCode == "" {
		return fmt.Errorf("override calculated threshold: core_node_name and payload_code required")
	}
	if overrideValue < 0 {
		return fmt.Errorf("override calculated threshold: override_value must be >= 0")
	}
	in.Source = "manual"
	in.ReplenishUOPThreshold = overrideValue
	return e.UpsertLoaderThreshold(in)
}

// ListLoaderClaimsForRecalculate returns the same set of (loader, payload)
// pairs the replenishment page renders — all styles, all processes — so
// the "Recalculate all" sweep covers every configurable binding, not
// just the currently-active style's. Backed by listAllLoaderBindings.
func (e *Engine) ListLoaderClaimsForRecalculate() ([]LoaderClaimPair, error) {
	return e.listAllLoaderBindings()
}

// LoaderClaimPair names one (loader, payload) binding eligible for
// the Recalculate-all sweep.
type LoaderClaimPair struct {
	CoreNodeName string `json:"core_node_name"`
	PayloadCode  string `json:"payload_code"`
	UOPCapacity  int    `json:"uop_capacity"`
}

// CellReorderInput is the write shape for the cell-side reorder_point
// + reorder_point_source pair.
type CellReorderInput struct {
	ClaimID      int64
	ReorderPoint int
	Source       string
	AutoReorder  bool
}

// SetPayloadCatalogCycleSeconds writes the engineer-edited per-part
// cycle time onto payload_catalog. Surfaced through the orchestration
// interface so the replenishment-page Apply path can land cycle + threshold
// in one round-trip.
func (e *Engine) SetPayloadCatalogCycleSeconds(payloadCode string, seconds float64) error {
	if payloadCode == "" {
		return fmt.Errorf("set payload cycle: payload_code required")
	}
	if seconds < 0 {
		return fmt.Errorf("set payload cycle: seconds must be >= 0")
	}
	return e.db.SetPayloadCatalogCycleSeconds(payloadCode, seconds)
}

// UpdateCellReorder modifies the reorder_point + source + AutoReorder
// fields on an existing style_node_claim. Other claim fields stay
// untouched — the engineer's edit on the replenishment page should
// not change InboundSource / OutboundDestination / etc.
func (e *Engine) UpdateCellReorder(in CellReorderInput) error {
	if in.ClaimID <= 0 {
		return fmt.Errorf("cell reorder: claim_id required")
	}
	if in.ReorderPoint < 0 {
		return fmt.Errorf("cell reorder: reorder_point must be >= 0")
	}
	current, err := e.db.GetStyleNodeClaim(in.ClaimID)
	if err != nil || current == nil {
		return fmt.Errorf("cell reorder: claim %d not found: %w", in.ClaimID, err)
	}
	source := in.Source
	if source == "" {
		source = "manual"
	}
	// Build NodeClaimInput from existing claim so we don't clobber
	// fields outside this admin path's concern.
	upd := processClaimToInput(current)
	upd.ReorderPoint = in.ReorderPoint
	upd.ReorderPointSource = source
	upd.AutoReorder = in.AutoReorder
	if _, err := e.db.UpsertStyleNodeClaim(upd); err != nil {
		return fmt.Errorf("cell reorder: upsert: %w", err)
	}
	return nil
}
