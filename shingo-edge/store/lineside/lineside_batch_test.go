package lineside

import (
	"reflect"
	"testing"
)

// The batched list must return exactly what the per-node calls return, node by
// node. Same discipline as the PayloadsForManualSwapNodes equivalence test: the
// batch exists purely to cut query count on a connection that serialises every
// read, so any behaviour difference is a bug rather than a trade-off.
func TestListForNodes_MatchesPerNodeCalls(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	// Node 100: two active buckets (two parts) plus a stranded one from StyleB.
	// Node 101: one active bucket only. A third id is asked for that has no
	// buckets at all, to pin the "absent from the map" case.
	if _, err := Capture(db, 100, "", 10, "P-500", 60); err != nil {
		t.Fatalf("capture 100/P-500: %v", err)
	}
	if _, err := Capture(db, 100, "", 10, "P-501", 25); err != nil {
		t.Fatalf("capture 100/P-501: %v", err)
	}
	if _, err := Capture(db, 100, "", 20, "P-900", 12); err != nil {
		t.Fatalf("capture 100/P-900: %v", err)
	}
	if err := DeactivateOtherStyles(db, 100, 10); err != nil {
		t.Fatalf("DeactivateOtherStyles: %v", err)
	}
	if _, err := Capture(db, 101, "", 10, "P-700", 40); err != nil {
		t.Fatalf("capture 101/P-700: %v", err)
	}

	nodeIDs := []int64{100, 101, 999}

	active, err := ListActiveForNodes(db, nodeIDs)
	if err != nil {
		t.Fatalf("ListActiveForNodes: %v", err)
	}
	inactive, err := ListInactiveForNodes(db, nodeIDs)
	if err != nil {
		t.Fatalf("ListInactiveForNodes: %v", err)
	}

	for _, id := range nodeIDs {
		wantActive, err := ListActiveForNode(db, id)
		if err != nil {
			t.Fatalf("ListActiveForNode(%d): %v", id, err)
		}
		if !equalBuckets(active[id], wantActive) {
			t.Errorf("active buckets for node %d:\n batched = %+v\n per-node = %+v", id, active[id], wantActive)
		}
		wantInactive, err := ListInactiveForNode(db, id)
		if err != nil {
			t.Fatalf("ListInactiveForNode(%d): %v", id, err)
		}
		if !equalBuckets(inactive[id], wantInactive) {
			t.Errorf("inactive buckets for node %d:\n batched = %+v\n per-node = %+v", id, inactive[id], wantInactive)
		}
	}

	// A node with no buckets must simply be absent, which reads the same as the
	// per-node form's empty slice at the call site.
	if got, ok := active[999]; ok {
		t.Errorf("node 999 has no buckets but appears in the active map: %+v", got)
	}
}

// A repeated node id must not duplicate that node's buckets. A station can list
// the same node twice — a changeover participant adopted as a child tile — so
// the id slice handed to the batch is not guaranteed to be unique.
func TestListForNodes_DeduplicatesNodeIDs(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	if _, err := Capture(db, 100, "", 10, "P-500", 60); err != nil {
		t.Fatalf("capture: %v", err)
	}

	once, err := ListActiveForNodes(db, []int64{100})
	if err != nil {
		t.Fatalf("ListActiveForNodes once: %v", err)
	}
	twice, err := ListActiveForNodes(db, []int64{100, 100, 100})
	if err != nil {
		t.Fatalf("ListActiveForNodes repeated: %v", err)
	}
	if !equalBuckets(once[100], twice[100]) {
		t.Errorf("repeated node id changed the result:\n once = %+v\n twice = %+v", once[100], twice[100])
	}
}

func TestListForNodes_EmptyInputRunsNoQuery(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	got, err := ListActiveForNodes(db, nil)
	if err != nil {
		t.Fatalf("ListActiveForNodes(nil): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListActiveForNodes(nil) = %+v, want empty", got)
	}
}

func equalBuckets(a, b []Bucket) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}
