// archtest_test.go — structural invariants enforced by grep across
// shingo-edge.
//
// These tests pin the UOP package's chokepoint invariants so future
// edits don't accidentally bypass the verb interface. Cheap CI test
// (no framework, just os.WalkDir + strings.Contains); runs in the
// uop package's test binary alongside accumulator_test.go.
package uop

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestArch_NoDirectRecordBinOrRecordBucket asserts that no production
// file outside shingo-edge/uop/ records a delta directly through the
// Sink's old raw record methods (deleted, both of them). Every emission
// must route through a named intent verb (Consumed, Produced, Fallthrough,
// CaptureToLineside, PilesChanged, ResendLevels — see mutator.go and
// the *.go files in this package). This keeps them from coming back.
//
// This invariant pins the value of the Phase 3a refactor: surfacing
// intent at the call site (the verb name documents the plant event)
// instead of having raw RecordBin/RecordBucket sprinkled with
// hand-picked reason strings.
//
// Test files (*_test.go) are exempt — the fakeDeltaSink in
// engine/wiring_counter_delta_test.go intentionally records and
// re-emits to track call shapes. The accumulator's own internal
// usage is also exempt (this file lives in uop/).
func TestArch_NoDirectRecordBinOrRecordBucket(t *testing.T) {
	t.Parallel()
	root := edgeRepoRoot(t)
	var bad []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip the uop package itself — accumulator.go uses
			// recordBin/recordBucket internally (lowercase, private).
			if filepath.Base(path) == "uop" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(data)
		for _, pat := range []string{
			"inventoryDelta.RecordBin",
			"inventoryDelta.RecordBucket",
		} {
			if strings.Contains(text, pat) {
				rel, _ := filepath.Rel(root, path)
				bad = append(bad, rel+" contains "+pat)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(bad) > 0 {
		t.Errorf("direct RecordBin/RecordBucket calls found outside uop/:\n  %s\n\n"+
			"Every delta emission must route through a named verb on uop.Mutator.\n"+
			"Add a new verb if needed; don't bypass.", strings.Join(bad, "\n  "))
	}
}

// TestArch_PileWritersAreKnown pins the set of production files that write
// node_lineside_bucket. Every write to a pile must mark its level dirty so Core's
// mirror follows (one writer set, one rule): the capture and the tick mark
// through their verbs, and the strand, the admin Clear and a process delete hand
// their keys to PilesChanged. The migrations run before the boot resend, which
// re-sends every row. A new writer outside this set is a pile Core never hears
// about; add it here only together with its mark.
func TestArch_PileWritersAreKnown(t *testing.T) {
	t.Parallel()
	root := edgeRepoRoot(t)
	allowed := map[string]bool{
		filepath.Join("store", "lineside", "lineside.go"):      true, // Capture, Drain, DeleteByID, StrandProcess
		filepath.Join("store", "processes", "processes.go"):    true, // Delete (engine sends the levels)
		filepath.Join("store", "migrations.go"):                true, // collapseDuplicateProcessNodes
		filepath.Join("store", "migrations_lineside_piles.go"): true, // the rebuild
	}
	var bad []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if allowed[rel] {
			return nil
		}
		text := string(data)
		for _, pat := range []string{
			"INSERT INTO node_lineside_bucket",
			"UPDATE node_lineside_bucket",
			"DELETE FROM node_lineside_bucket",
		} {
			if strings.Contains(text, pat) {
				bad = append(bad, rel+" contains "+pat)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(bad) > 0 {
		t.Errorf("node_lineside_bucket written outside the known writer set:\n  %s\n\n"+
			"A pile write must mark its level dirty (uop.Mutator.PilesChanged or a capture/tick verb).",
			strings.Join(bad, "\n  "))
	}
}

// TestArch_ActiveBinPointerWritersAreKnown pins the set of production files
// that write the process_node_runtime_states slot-lifecycle columns (the
// active-bin pointer, the cached count, the stamp) through the raw store
// setters. Engine code must go through the uop verbs instead — the verb name
// is the plant event at the call site. The one engine file still allowed is a
// documented exception: handler_bin_epoch_refresh.go is a stamp-only refresh
// (the bin stays; no identity or count changes hands). handler_uop_adjustment.go
// used to be allowed for its bind arms; they go through BindFromCore and
// BindStagedUnlessDeparted now, and the file left the list.
func TestArch_ActiveBinPointerWritersAreKnown(t *testing.T) {
	t.Parallel()
	root := edgeRepoRoot(t)
	allowed := map[string]bool{
		filepath.Join("uop", "mutator.go"):                      true, // the verbs
		filepath.Join("uop", "store_iface.go"):                  true, // the surface they write through
		filepath.Join("store", "process_node_runtime.go"):       true, // the delegates
		filepath.Join("engine", "handler_bin_epoch_refresh.go"): true, // stamp-only refresh, documented
	}
	patterns := []string{
		".BindEmptySlotUnlessDeparted(",
		"SetProcessNodeActiveBinID(",
		"SetProcessNodeActiveBinIDAndEpoch(",
		"ClearProcessNodeActiveBinAndCount(",
		"SetProcessNodeRuntime(",
		"SetProcessNodeRuntimeWithBin(",
		"SetProcessNodeRuntimeWithBinAndEpoch(",
		"SetProcessNodeRuntimeClaimCountAndEpoch(",
		"SetProcessNodeRuntimeForDeliveredBin(",
	}

	var bad []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		if allowed[rel] {
			return nil
		}
		text := string(data)
		for _, pat := range patterns {
			if strings.Contains(text, pat) {
				bad = append(bad, rel+" calls "+pat+"...) directly")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(bad) > 0 {
		t.Errorf("runtime slot columns written outside the known writer set:\n  %s\n\n"+
			"Route the write through a uop verb (mutator.go) so the plant event is named at "+
			"the call site and the lineside identity moves with the carrier.",
			strings.Join(bad, "\n  "))
	}
}

// TestArch_ResidentPayloadHasOneDoorway pins the chokepoint. The resident
// identity is written from exactly one place, so "who last said what this
// carrier is" always has an answer.
//
// The field it replaces, active_claim_id, is written by eighteen paths, most
// of them stamping the requested style, with nothing recording which spoke
// last. That is how an ordinary recovery action came to re-aim a cell's idea
// of what was standing on it.
//
// Two patterns: the store method the doorway calls, and the SQL column
// itself. The column is assigned in exactly ONE statement, the doorway's own
// (processes.SetRuntimeLinesidePayload). Departures and clears used to write
// it as literals inside the pointer statements, which bypassed the doorway
// and its "who said so"; those literals are gone and the count below keeps
// them from coming back.
func TestArch_ResidentPayloadHasOneDoorway(t *testing.T) {
	t.Parallel()
	root := edgeRepoRoot(t)
	patterns := map[string]map[string]bool{
		"SetProcessNodeRuntimeLinesidePayload": {
			filepath.Join("engine", "lineside_carrier_doorway.go"): true,
			filepath.Join("store", "process_node_runtime.go"):      true,
		},
		"lineside_payload_code=": {
			filepath.Join("store", "processes", "processes.go"): true,
		},
	}

	var bad []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(root, path)
		if rel == filepath.Join("store", "processes", "processes.go") {
			if n := strings.Count(string(data), "lineside_payload_code="); n != 1 {
				bad = append(bad, fmt.Sprintf("%s assigns lineside_payload_code= in %d statements, want 1 "+
					"(the doorway's SetRuntimeLinesidePayload)", rel, n))
			}
		}
		for pat, patAllowed := range patterns {
			if !strings.Contains(string(data), pat) {
				continue
			}
			if !patAllowed[rel] {
				bad = append(bad, rel+" writes "+pat)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(bad) > 0 {
		t.Errorf("the lineside identity is written outside its doorway:\n  %s\n\n"+
			"Route it through Engine.recordLinesideCarrier and name the source — a departure "+
			"goes through Engine.carrierLeft, which calls it. A field several paths write "+
			"without saying which spoke last is the defect this replaced, not a shape to "+
			"reintroduce.",
			strings.Join(bad, "\n  "))
	}
}

// TestArch_CarrierLeavesThroughOneVerb pins "the carrier left" to one verb.
// Engine.carrierLeft (engine/carrier_left.go) clears the pointer and the count
// through uop.ClearActiveBin and records the departure through the lineside
// doorway; no other production file may call ClearActiveBin on the sink, or
// record a departure on its own. A second caller is a door that nulls
// active_bin_id and can forget the identity — the defect the verb exists to
// end. (Tests are exempt: they drive the Mutator directly.)
func TestArch_CarrierLeavesThroughOneVerb(t *testing.T) {
	t.Parallel()
	root := edgeRepoRoot(t)
	verb := filepath.Join("engine", "carrier_left.go")
	patterns := []string{
		".ClearActiveBin(",
		"domain.CarrierDeparted",
	}
	var bad []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if filepath.Base(path) == "uop" {
				return filepath.SkipDir // the verb's pointer/count half lives here
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		if rel == verb {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for _, pat := range patterns {
			if strings.Contains(string(data), pat) {
				bad = append(bad, rel+" uses "+pat)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(bad) > 0 {
		t.Errorf("a carrier departure outside the one verb:\n  %s\n\n"+
			"Call Engine.carrierLeft — it clears the pointer and the count and records the "+
			"departure through the lineside doorway, so the identity cannot be left behind.",
			strings.Join(bad, "\n  "))
	}
}

// edgeRepoRoot returns the absolute path to shingo-edge/ by walking
// upward from the test's CWD until it finds a go.mod whose first line
// declares the shingoedge module.
func edgeRepoRoot(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := cwd
	for {
		modPath := filepath.Join(dir, "go.mod")
		if data, err := os.ReadFile(modPath); err == nil {
			if strings.Contains(string(data), "module shingoedge") {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no shingoedge go.mod found walking up from %s", cwd)
		}
		dir = parent
	}
}
