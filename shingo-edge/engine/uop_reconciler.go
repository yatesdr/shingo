// uop_reconciler.go — UOP reconciliation against Core's authoritative
// state.
//
// Two responsibilities:
//
//   1. Bin self-heal. For every bin Core lists at this station's
//      nodes, read the authoritative uop_remaining and overwrite the
//      local process_node_runtime_states.remaining_uop cache from it.
//      Core wins. Tri-state error handling: when Core can't be
//      reached, retain prior cached value rather than zeroing.
//
//   2. Bucket drift. For every authoritative bucket Core knows about
//      for this station, compare against Edge's local
//      node_lineside_bucket. local.qty != core.qty surfaces as drift
//      via the metrics counter — either a delta got lost in flight or
//      was applied wrong. Local-only buckets (Core's stream missed
//      them) likewise surface.
//
// Cadence: piggybacks the existing Core sync surface — no new
// goroutine; fires inside the periodic Core-sync flow with a
// since-last-pass gate. Operators trigger immediate passes via
// Engine.Reconcile(true).
package engine

import (
	"sync"
	"sync/atomic"
	"time"

	"shingoedge/store/lineside"
	"shingoedge/store/processes"
)

// uopReconciler holds the gating state for the reconciler's
// since-last-pass check. Lives on Engine; not exported.
type uopReconciler struct {
	mu       sync.Mutex
	interval time.Duration
	last     time.Time

	// metrics: cumulative counters since process start. Updated under
	// mu in advance order with the timestamp; readable via Engine.
	// UOPReconcilerMetrics for ops-side ad-hoc inspection.
	binsSeen        atomic.Int64
	binsHealed      atomic.Int64
	binsSkipped     atomic.Int64
	bucketsSeen     atomic.Int64
	bucketsDrifted  atomic.Int64
	bucketsHealed   atomic.Int64
	bucketsSkipped  atomic.Int64
	passes          atomic.Int64
}

// SetReconcileInterval overrides the minimum gap between reconciliation
// passes. The composition root reads it from config (uop.reconcile_interval)
// and calls this once at startup; safe to call any time (the next
// Reconcile pass picks it up). When interval ends up zero the gate
// effectively disables — every Reconcile call runs.
func (e *Engine) SetReconcileInterval(d time.Duration) {
	if d <= 0 {
		return
	}
	if e.uopReconciler == nil {
		e.uopReconciler = &uopReconciler{interval: d}
		return
	}
	e.uopReconciler.mu.Lock()
	e.uopReconciler.interval = d
	e.uopReconciler.mu.Unlock()
}

// UOPReconcilerMetrics is a lightweight snapshot of cumulative
// reconciler activity. Returned by ReconcilerMetrics for ops-side
// drift visibility.
//
// BinsSkipped counts bins the reconciler observed but skipped because
// the inventory delta reporter reported them as pending (in-flight).
// Healthy steady state: skip count grows roughly at the rate of
// operator releases / tick-driven flushes; sustained skip-spike
// indicates a flush stall.
type UOPReconcilerMetrics struct {
	Passes         int64     `json:"passes"`
	BinsSeen       int64     `json:"bins_seen"`
	BinsHealed     int64     `json:"bins_healed"`
	BinsSkipped    int64     `json:"bins_skipped"`
	BucketsSeen    int64     `json:"buckets_seen"`
	BucketsDrifted int64     `json:"buckets_drifted"`
	BucketsHealed  int64     `json:"buckets_healed"`
	BucketsSkipped int64     `json:"buckets_skipped"`
	// FlushFailures is sourced from the InventoryDeltaSink (Item 9).
	// Sustained growth signals the outbox is wedged — pending-delta
	// guard will start blocking releases.
	FlushFailures int64     `json:"flush_failures"`
	LastPass      time.Time `json:"last_pass"`
}

// ReconcilerMetrics returns the cumulative reconciler counters since
// process start.
func (e *Engine) ReconcilerMetrics() UOPReconcilerMetrics {
	r := e.uopReconciler
	if r == nil {
		return UOPReconcilerMetrics{}
	}
	r.mu.Lock()
	last := r.last
	r.mu.Unlock()
	var flushFailures int64
	if e.inventoryDelta != nil {
		flushFailures = e.inventoryDelta.FlushFailures()
	}
	return UOPReconcilerMetrics{
		Passes:         r.passes.Load(),
		BinsSeen:       r.binsSeen.Load(),
		BinsHealed:     r.binsHealed.Load(),
		BinsSkipped:    r.binsSkipped.Load(),
		BucketsSeen:    r.bucketsSeen.Load(),
		BucketsDrifted: r.bucketsDrifted.Load(),
		BucketsHealed:  r.bucketsHealed.Load(),
		BucketsSkipped: r.bucketsSkipped.Load(),
		FlushFailures:  flushFailures,
		LastPass:       last,
	}
}

// Reconcile runs one reconciliation pass, gated by the since-last-pass
// interval. force=true bypasses the gate (use for operator-triggered
// immediate passes); production sweeps pass force=false and rely on
// the gate to coalesce frequent triggers.
//
// Errors are logged and absorbed — a transient Core blip does not
// fail the pass, it just produces a "no work this pass" log line.
// Self-heal writes are best-effort (per-bin / per-bucket); a failed
// heal logs and the next pass retries.
func (e *Engine) Reconcile(force bool) {
	if e.uopReconciler == nil {
		e.uopReconciler = &uopReconciler{}
	}
	r := e.uopReconciler

	r.mu.Lock()
	interval := r.interval
	now := time.Now()
	if !force && !r.last.IsZero() && now.Sub(r.last) < interval {
		r.mu.Unlock()
		return
	}
	r.last = now
	r.mu.Unlock()

	r.passes.Add(1)
	e.runReconciliationPass()
}

// runReconciliationPass does one observation cycle. Pulls the
// shadow snapshot from Core for this station's nodes, then walks
// each result against the local view to log drift.
func (e *Engine) runReconciliationPass() {
	if e.coreClient == nil || !e.coreClient.Available() {
		e.logFn("uop_reconciler: skipped, core API unavailable")
		return
	}

	nodes, err := e.db.ListProcessNodes()
	if err != nil {
		e.logFn("uop_reconciler: list process nodes: %v", err)
		return
	}
	if len(nodes) == 0 {
		return
	}
	nodeNames := make([]string, 0, len(nodes))
	nodeByName := make(map[string]*processes.Node, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		if n.CoreNodeName == "" {
			continue
		}
		nodeNames = append(nodeNames, n.CoreNodeName)
		nodeByName[n.CoreNodeName] = n
	}
	if len(nodeNames) == 0 {
		return
	}

	station := ""
	if e.cfg != nil {
		station = e.cfg.StationID()
	}
	snapshot, err := e.coreClient.FetchUOPState(station, nodeNames)
	if err != nil || snapshot == nil {
		// FetchUOPState returns (nil, nil) when Core is
		// unreachable. Either way, nothing to compare against —
		// log a marker and return. Self-heal mode preserves prior
		// cached value via this early-return (no writes happen on
		// this pass).
		e.logFn("uop_reconciler: state fetch returned no data")
		return
	}

	r := e.uopReconciler

	returnedNodes := make(map[string]bool, len(snapshot.Bins))
	for _, b := range snapshot.Bins {
		returnedNodes[b.NodeName] = true
		r.binsSeen.Add(1)

		localNode, ok := nodeByName[b.NodeName]
		if !ok {
			continue
		}

		// Pending-delta guard: while the inventory delta reporter
		// has unflushed/unsent activity for this bin, Core's
		// snapshot is stale relative to Edge's pipeline. Healing
		// would stomp the local runtime cache with a value that
		// pre-dates the in-flight delta. Skip this bin and let the
		// next pass try once the delta lands.
		if e.inventoryDelta != nil && e.inventoryDelta.IsPendingBinDelta(b.BinID) {
			r.binsSkipped.Add(1)
			e.logFn("uop_reconciler: skip-pending bin=%d node=%s",
				b.BinID, b.NodeName)
			continue
		}

		healed, err := e.healLocalRuntimeFromBin(localNode, b)
		if err != nil {
			e.logFn("uop_reconciler: self-heal node=%s: %v", b.NodeName, err)
			continue
		}
		if healed {
			r.binsHealed.Add(1)
		}
	}

	// Item 4: empty-slot detection. For every node Edge tracks but
	// Core's snapshot didn't return, the slot is confirmed empty
	// (FetchUOPState already early-returned at the unreachable case
	// further up, so reaching here means Core was reachable AND chose
	// not to include the node). The cached runtime should reflect
	// that. Without this pass, stations whose bin walked off without
	// a normal completion event show stale UOP indefinitely.
	for nodeName, localNode := range nodeByName {
		if returnedNodes[nodeName] {
			continue
		}
		runtime, err := e.db.GetProcessNodeRuntime(localNode.ID)
		if err != nil || runtime == nil || runtime.RemainingUOPCached == 0 {
			continue
		}
		if err := e.healLocalRuntimeEmpty(localNode); err != nil {
			e.logFn("uop_reconciler: heal-empty node=%s: %v", nodeName, err)
			continue
		}
		r.binsHealed.Add(1)
		e.logFn("uop_reconciler: heal-empty node=%s runtime=%d → 0 (Core reports no bin at slot)",
			nodeName, runtime.RemainingUOPCached)
	}

	// Group local buckets by node id for an O(1) lookup against the
	// shadow rows. ListLinesideBuckets returns active+inactive in one
	// call so the comparison covers both states.
	localByKey := make(map[bucketKey]*lineside.Bucket)
	for _, n := range nodeByName {
		buckets, err := e.db.ListLinesideBuckets(n.ID)
		if err != nil {
			e.logFn("uop_reconciler: list local buckets node=%d: %v", n.ID, err)
			continue
		}
		for i := range buckets {
			b := buckets[i]
			k := bucketKey{
				nodeID:     n.ID,
				pairKey:    b.PairKey,
				styleID:    b.StyleID,
				partNumber: b.PartNumber,
			}
			localByKey[k] = &b
		}
	}

	for _, sh := range snapshot.Buckets {
		// Core's NodeID is its own internal id, not Edge's. Look up
		// by name to get the local node id, then compose the key.
		localNode, ok := nodeByName[sh.NodeName]
		if !ok {
			// Core knows about a bucket on a node Edge doesn't track
			// — possibly a stale cross-station shadow row, possibly
			// a node that was renamed. Surface as drift so it's
			// visible.
			r.bucketsSeen.Add(1)
			r.bucketsDrifted.Add(1)
			e.logFn("uop_reconciler: bucket drift orphaned shadow row node=%s part=%s qty=%d (no matching local node)",
				sh.NodeName, sh.PartNumber, sh.Qty)
			continue
		}
		r.bucketsSeen.Add(1)
		k := bucketKey{
			nodeID:     localNode.ID,
			pairKey:    sh.PairKey,
			styleID:    sh.StyleID,
			partNumber: sh.PartNumber,
		}
		local := localByKey[k]
		var localQty int
		if local != nil {
			localQty = local.Qty
		}
		drift := localQty - sh.Qty
		if drift != 0 {
			r.bucketsDrifted.Add(1)
			e.logFn("uop_reconciler: bucket drift node=%s part=%s style=%d local=%d shadow=%d drift=%+d",
				sh.NodeName, sh.PartNumber, sh.StyleID, localQty, sh.Qty, drift)

			// Item 5: bucket self-heal. Pending-delta guard mirrors
			// the bin path — skip when the reporter has unflushed
			// activity on this scope so we don't stomp an in-flight
			// bucket delta with a stale Core read.
			if e.inventoryDelta != nil && e.inventoryDelta.IsPendingBucketDelta(localNode.ID, sh.StyleID, sh.PartNumber) {
				r.bucketsSkipped.Add(1)
				e.logFn("uop_reconciler: skip-pending bucket node=%s part=%s",
					sh.NodeName, sh.PartNumber)
			} else if err := e.healLocalBucketFromCore(localNode, sh); err != nil {
				e.logFn("uop_reconciler: heal-bucket node=%s part=%s: %v",
					sh.NodeName, sh.PartNumber, err)
			} else {
				r.bucketsHealed.Add(1)
			}
		}
		// Mark this scope as seen so we can detect local-only
		// buckets (present on Edge, missing from Core's shadow) in
		// the second pass below.
		delete(localByKey, k)
	}

	// Anything left in localByKey is a local bucket Core's shadow
	// doesn't know about — either a delta in flight or a delta that
	// got lost. Either way, drift signal — and heal toward Core
	// (delete the local row) unless a pending delta says otherwise.
	for _, b := range localByKey {
		if b.Qty == 0 {
			continue
		}
		r.bucketsSeen.Add(1)
		r.bucketsDrifted.Add(1)
		e.logFn("uop_reconciler: bucket drift local-only node=%d part=%s style=%d local=%d shadow=missing",
			b.NodeID, b.PartNumber, b.StyleID, b.Qty)
		if e.inventoryDelta != nil && e.inventoryDelta.IsPendingBucketDelta(b.NodeID, b.StyleID, b.PartNumber) {
			r.bucketsSkipped.Add(1)
			e.logFn("uop_reconciler: skip-pending local-only bucket node=%d part=%s",
				b.NodeID, b.PartNumber)
			continue
		}
		if err := e.db.SetLinesideBucketForReconcile(b.NodeID, b.PairKey, b.StyleID, b.PartNumber, 0); err != nil {
			e.logFn("uop_reconciler: heal-bucket-delete node=%d part=%s: %v",
				b.NodeID, b.PartNumber, err)
			continue
		}
		r.bucketsHealed.Add(1)
	}
}

// healLocalBucketFromCore overwrites the local bucket qty from Core's
// authoritative shadow row. Symmetric with healLocalRuntimeFromBin
// for the bin side. Caller must have checked the pending-delta guard
// before calling.
func (e *Engine) healLocalBucketFromCore(node *processes.Node, sh LinesideBucketRow) error {
	return e.db.SetLinesideBucketForReconcile(node.ID, sh.PairKey, sh.StyleID, sh.PartNumber, sh.Qty)
}

// bucketKey composites the four fields that identify a unique
// (Core-side or Edge-side) bucket. Used to join Edge's local view
// against Core's shadow rows in O(N+M).
type bucketKey struct {
	nodeID     int64
	pairKey    string
	styleID    int64
	partNumber string
}

// healLocalRuntimeEmpty zeroes the local runtime cache for a node
// that Core's snapshot confirmed has no bin sitting at the slot.
// Item 4: handles the "bin walked off without a normal completion
// event" scenario — without this, the slot's stale UOP lingers on
// the operator UI indefinitely. ActiveClaimID stays nil so a future
// FetchUOPState that DOES report a bin can re-anchor the claim.
func (e *Engine) healLocalRuntimeEmpty(node *processes.Node) error {
	if err := e.db.SetProcessNodeRuntime(node.ID, nil, 0); err != nil {
		return err
	}
	return nil
}

// healLocalRuntimeFromBin overwrites the local
// process_node_runtime_states.remaining_uop cache from Core's
// authoritative bin count. Core wins.
//
// Returns (healed, err): healed=true means a write actually landed
// (caller bumps BinsHealed); healed=false with err=nil means values
// were already in lockstep, no work, no metric movement. Without
// the boolean, BinsHealed would tick up on every pass against a
// steady-state slot — drowning genuine drift signals in noise.
//
// Tri-state error handling: when Core's batch response was missing
// the node entirely, runReconciliationPass exits before reaching
// here, so the local cache retains its prior value rather than
// being zeroed by a transient blip. The bin parameter is the row
// already in hand from the uop-state batch fetch.
func (e *Engine) healLocalRuntimeFromBin(node *processes.Node, bin BinUOPRow) (bool, error) {
	runtime, err := e.db.GetProcessNodeRuntime(node.ID)
	if err != nil {
		return false, err
	}
	if runtime == nil {
		// No runtime row to heal — nothing to overwrite.
		return false, nil
	}
	target := bin.UOPRemaining
	if runtime.RemainingUOPCached == target {
		// Already in lockstep — no write needed.
		return false, nil
	}
	if err := e.db.SetProcessNodeRuntime(node.ID, runtime.ActiveClaimID, target); err != nil {
		return false, err
	}
	e.logFn("uop_reconciler: self-heal node=%s runtime=%d → %d (Core authoritative)",
		node.CoreNodeName, runtime.RemainingUOPCached, target)
	return true, nil
}
