//go:build docker

package service

import (
	"context"
	"fmt"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// BenchmarkSystemUOPForPayload measures the authoritative in-loop-UOP read at
// plant scale. This is the GATE for the monitor-tally collapse: the threshold
// monitor is moving from a private incremental cache to reading this sum
// directly on every evaluation, so the read has to be cheap enough to sit on
// the consume-tick hot path.
//
// Synthetic scale (≥500 bins, ≥50 payloads, buckets included):
//   - 60 payloads
//   - 720 bins spread across those payloads and 12 nodes, with a realistic
//     status mix (available / staged plus a slice of the excluded
//     flagged/maintenance/quality_hold/retired states so the lifecycle filter
//     is exercised, not short-circuited)
//   - active lineside piles on ~half the payloads at consuming nodes (the
//     bucket arm is SUM(qty) over state='active'; it read the plant-claims
//     mirror through two correlated EXISTS until the level wire, v131)
//
// Two sub-benchmarks:
//   - single_payload: the monitor's hot-path shape — one payload per call,
//     which is what OnBinUOPDelta / OnBucketApplied now issue per delta. THIS
//     is the number the gate is read against.
//   - all_payloads: the ReplenishmentHealth page shape — every payload in one
//     call — for reference.
func BenchmarkSystemUOPForPayload(b *testing.B) {
	db := testdb.Open(b)
	svc := NewInventoryService(db)
	payloads := seedPlantScale(b, db)
	ctx := context.Background()

	// Warm once so the first timed iteration isn't paying plan-cache costs.
	if _, err := svc.SystemUOPForPayload(ctx, payloads); err != nil {
		b.Fatalf("warm-up SystemUOPForPayload: %v", err)
	}

	b.Run("single_payload", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			p := payloads[i%len(payloads)]
			if _, err := svc.SystemUOPForPayload(ctx, []string{p}); err != nil {
				b.Fatalf("SystemUOPForPayload(%s): %v", p, err)
			}
		}
	})

	b.Run("all_payloads", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := svc.SystemUOPForPayload(ctx, payloads); err != nil {
				b.Fatalf("SystemUOPForPayload(batch): %v", err)
			}
		}
	})
}

// seedPlantScale populates the DB with plant-scale synthetic inventory and
// returns the payload codes. Uses store primitives + direct inserts (no
// *testing.T-only helpers) so it runs under a benchmark's testing.B.
func seedPlantScale(b *testing.B, db *store.DB) []string {
	b.Helper()

	const (
		numPayloads = 60
		numNodes    = 12
		numBins     = 720
	)

	// One bin type for every bin.
	bt, err := db.GetBinTypeByCode("DEFAULT")
	if err != nil {
		bt = &bins.BinType{Code: "DEFAULT", Description: "Bench default bin type"}
		if err := db.CreateBinType(bt); err != nil {
			b.Fatalf("create bin type: %v", err)
		}
	}

	// Nodes: the first 6 are consuming lineside nodes (buckets + active-style
	// mirror), the rest are plain storage.
	nodeNames := make([]string, numNodes)
	nodeIDs := make([]int64, numNodes)
	for i := 0; i < numNodes; i++ {
		name := fmt.Sprintf("BENCH_NODE_%02d", i)
		n := &nodes.Node{Name: name, Enabled: true}
		if err := db.CreateNode(n); err != nil {
			b.Fatalf("create node %s: %v", name, err)
		}
		nodeNames[i] = name
		nodeIDs[i] = n.ID
	}

	payloads := make([]string, numPayloads)
	for i := range payloads {
		payloads[i] = fmt.Sprintf("BENCH-PAYLOAD-%03d", i)
	}

	// Bins: round-robin payload × node, rotating status so the lifecycle
	// filter and the staged-counts-in-full path both see real rows. Direct
	// inserts — SystemUOPForPayload reads only (payload_code, status,
	// uop_remaining), so the manifest-confirm flow is unnecessary here.
	statuses := []string{"available", "available", "available", "staged", "flagged", "maintenance", "quality_hold", "retired"}
	for i := 0; i < numBins; i++ {
		payload := payloads[i%numPayloads]
		nodeID := nodeIDs[i%numNodes]
		status := statuses[i%len(statuses)]
		uop := 20 + (i % 130) // 20..149
		if _, err := db.Exec(
			`INSERT INTO bins (bin_type_id, label, node_id, status, payload_code, uop_remaining)
			 VALUES ($1,$2,$3,$4,$5,$6)`,
			bt.ID, fmt.Sprintf("BENCH-BIN-%04d", i), nodeID, status, payload, uop,
		); err != nil {
			b.Fatalf("insert bin %d: %v", i, err)
		}
	}

	// Lineside buckets on the consuming nodes for the first half of payloads.
	for i := 0; i < numPayloads/2; i++ {
		payload := payloads[i]
		node := nodeNames[i%6]
		if _, err := db.Exec(
			`INSERT INTO lineside_buckets (station, core_node_name, payload_code, state, qty)
			 VALUES ($1,$2,$3,'active',$4)`,
			"bench-station", node, payload, 40+i,
		); err != nil {
			b.Fatalf("insert bucket for %s@%s: %v", payload, node, err)
		}
	}

	return payloads
}
