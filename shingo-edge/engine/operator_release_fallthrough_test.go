// operator_release_fallthrough_test.go — tests that exercise the
// disposition-skip log lines added during the release-flow cleanup.
//
// Background: ReleaseOrderWithLineside has four code paths that legitimately
// skip the manifest-sync step (no process node, no active claim, produce
// role, supply-bin guard). Pre-cleanup, two of them returned silently —
// which meant a "the prompt didn't fire" investigation that landed on one
// of those paths had no breadcrumb to grep for. Post-cleanup, every skip
// site emits a structured log line with a stable suffix that names the
// skip reason.
//
// These tests assert the breadcrumbs exist and contain enough context to
// diagnose the next 04-27-style incident without re-reading the engine.
package engine

import (
	"fmt"
	"strings"
	"testing"

	"shingoedge/orders"
	"shingoedge/store/processes"
)

// captureLogs builds a logFn that appends formatted lines to *out. Returns
// the function so tests can swap it in via eng.logFn = captureLogs(&out).
func captureLogs(out *[]string) func(string, ...interface{}) {
	return func(format string, args ...interface{}) {
		*out = append(*out, fmt.Sprintf(format, args...))
	}
}

// containsAll reports whether haystack contains every needle. Avoids the
// "did the test grep for the wrong fragment" failure mode by spelling out
// each required token in the assertion site.
func containsAll(haystack string, needles ...string) bool {
	for _, n := range needles {
		if !strings.Contains(haystack, n) {
			return false
		}
	}
	return true
}

// findLogLine returns the first line in logs that contains every needle,
// or "" if none matched. Test assertions read better with this helper than
// with manual loops.
func findLogLine(logs []string, needles ...string) string {
	for _, l := range logs {
		if containsAll(l, needles...) {
			return l
		}
	}
	return ""
}

// TestReleaseOrderWithLineside_NoProcessNode_LogsSkip verifies that an
// order without a process node — the pure-kanban / generic-move case —
// emits a "no_process_node" breadcrumb when its disposition is dropped.
//
// Pre-cleanup this branch returned silently, leaving a future investigator
// to wonder why a release click produced no manifest-sync side effect.
// Post-cleanup, the breadcrumb names the skip reason and surfaces the
// disposition the operator declared.
func TestReleaseOrderWithLineside_NoProcessNode_LogsSkip(t *testing.T) {
	db := testEngineDB(t)
	eng := testEngine(t, db)
	var logs []string
	eng.logFn = captureLogs(&logs)

	// Create a kanban-style order with no process node attached.
	orderID, err := db.CreateOrder("uuid-no-pn", orders.TypeRetrieve,
		nil /* processNodeID */, false, 1, "GENERIC-NODE", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := db.UpdateOrderStatus(orderID, string(orders.StatusStaged)); err != nil {
		t.Fatalf("transition to staged: %v", err)
	}

	disp := ReleaseDisposition{
		Mode:     DispositionCaptureLineside,
		CalledBy: "stephen-station-test",
	}
	if err := eng.ReleaseOrderWithLineside(orderID, disp); err != nil {
		t.Fatalf("ReleaseOrderWithLineside: %v", err)
	}

	line := findLogLine(logs, "release:", "no_process_node",
		"capture_lineside", fmt.Sprintf("order=%d", orderID))
	if line == "" {
		t.Errorf("no_process_node log line not found.\nLogs were:\n  %s",
			strings.Join(logs, "\n  "))
	}
}

// TestReleaseOrderWithLineside_ProduceRole_LogsSkip verifies that a
// release on a produce-role node emits a "produce_role" breadcrumb. The
// produce role doesn't use lineside buckets and doesn't reset UOP on
// release (it resets on ingest completion), so the disposition is
// intentionally dropped — but the skip is now visible in the log.
func TestReleaseOrderWithLineside_ProduceRole_LogsSkip(t *testing.T) {
	db := testEngineDB(t)
	_, nodeID, _, _ := seedProduceNode(t, db, "simple")
	eng := testEngine(t, db)
	var logs []string
	eng.logFn = captureLogs(&logs)

	// Stage an order against the produce node.
	orderID, err := db.CreateOrder("uuid-produce-rel", orders.TypeComplex,
		&nodeID, false, 1, "PRODUCE-NODE", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := db.UpdateOrderStatus(orderID, string(orders.StatusStaged)); err != nil {
		t.Fatalf("transition to staged: %v", err)
	}

	disp := ReleaseDisposition{
		Mode:     DispositionSendPartialBack,
		CalledBy: "stephen-station-test",
	}
	if err := eng.ReleaseOrderWithLineside(orderID, disp); err != nil {
		t.Fatalf("ReleaseOrderWithLineside: %v", err)
	}

	line := findLogLine(logs, "release:", "produce_role",
		"send_partial_back", fmt.Sprintf("order=%d", orderID),
		fmt.Sprintf("node=%s", "Produce Node"))
	if line == "" {
		t.Errorf("produce_role log line not found.\nLogs were:\n  %s",
			strings.Join(logs, "\n  "))
	}
}

// TestReleaseOrderWithLineside_NoActiveClaim_LogsSkip verifies the existing
// toClaim==nil log keeps firing post-cleanup. The supply-order tests
// modified by this PR could mask a regression in the nil-claim log; this
// test is belt-and-suspenders coverage.
//
// Setup: a process node with a node entry but no active claim — so
// resolveReleaseClaim returns nil. The release should still succeed, log
// the disposition, and call orderMgr.ReleaseOrder with nil remaining_uop.
func TestReleaseOrderWithLineside_NoActiveClaim_LogsSkip(t *testing.T) {
	db := testEngineDB(t)

	// Process + node with no claim. Mirrors a misconfigured-station
	// scenario: the node row exists but no claim row points at it, so
	// runtime.ActiveClaimID is nil and resolveReleaseClaim returns nil.
	processID, err := db.CreateProcess("ORPHAN-PROC", "orphan", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: "ORPHAN-NODE",
		Code:         "ON1",
		Name:         "Orphan Node",
		Sequence:     1,
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	if _, err := db.EnsureProcessNodeRuntime(nodeID); err != nil {
		t.Fatalf("ensure runtime: %v", err)
	}

	orderID, err := db.CreateOrder("uuid-orphan-rel", orders.TypeComplex,
		&nodeID, false, 1, "ORPHAN-NODE", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := db.UpdateOrderStatus(orderID, string(orders.StatusStaged)); err != nil {
		t.Fatalf("transition to staged: %v", err)
	}

	eng := testEngine(t, db)
	var logs []string
	eng.logFn = captureLogs(&logs)

	disp := ReleaseDisposition{
		Mode:     DispositionCaptureLineside,
		CalledBy: "stephen-station-test",
	}
	if err := eng.ReleaseOrderWithLineside(orderID, disp); err != nil {
		t.Fatalf("ReleaseOrderWithLineside: %v", err)
	}

	// Existing log line shape (pre-cleanup): "toClaim is nil ... disposition %q dropped"
	line := findLogLine(logs, "toClaim is nil", "capture_lineside",
		fmt.Sprintf("%d", orderID))
	if line == "" {
		t.Errorf("toClaim==nil log line not found.\nLogs were:\n  %s",
			strings.Join(logs, "\n  "))
	}
}

// Sanity check: ensure every fall-through log line from this PR carries
// both an order ID and the disposition mode. A breadcrumb without those
// two facts isn't grep-able to a specific incident — the whole point of
// the cleanup. If a future edit to operator_release.go drops one, this
// test catches it before review.
func TestReleaseOrderWithLineside_FallthroughLogShape_IncludesOrderAndDisposition(t *testing.T) {
	db := testEngineDB(t)
	eng := testEngine(t, db)
	var logs []string
	eng.logFn = captureLogs(&logs)

	// Drive the no_process_node path.
	orderID, err := db.CreateOrder("uuid-shape", orders.TypeRetrieve,
		nil, false, 1, "GENERIC", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := db.UpdateOrderStatus(orderID, string(orders.StatusStaged)); err != nil {
		t.Fatalf("transition to staged: %v", err)
	}
	if err := eng.ReleaseOrderWithLineside(orderID,
		ReleaseDisposition{Mode: DispositionCaptureLineside, CalledBy: "t"}); err != nil {
		t.Fatalf("ReleaseOrderWithLineside: %v", err)
	}

	if len(logs) == 0 {
		t.Fatal("expected at least one fall-through log line, got none")
	}
	for _, line := range logs {
		if !strings.Contains(line, fmt.Sprintf("order=%d", orderID)) {
			t.Errorf("log line missing order=%d: %q", orderID, line)
		}
		if !strings.Contains(line, "disposition=") {
			t.Errorf("log line missing disposition= field: %q", line)
		}
	}
}
