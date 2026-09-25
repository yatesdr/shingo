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

	// Node 100: two active piles (two parts) plus a stranded one.
	// Node 101: one active pile only. A third id is asked for that has no
	// piles at all, to pin the "absent from the map" case.
	for _, c := range []struct {
		node int64
		part string
		qty  int
	}{{100, "P-500", 60}, {100, "P-501", 25}, {101, "P-700", 40}} {
		if _, err := Capture(db, c.node, c.part, c.qty); err != nil {
			t.Fatalf("capture %d/%s: %v", c.node, c.part, err)
		}
	}
	seedStranded(t, db, 100, "P-900", 12)

	nodeIDs := []int64{100, 101, 999}

	active, err := ListActiveForNodes(db, nodeIDs)
	if err != nil {
		t.Fatalf("ListActiveForNodes: %v", err)
	}
	stranded, err := ListStrandedForNodes(db, nodeIDs)
	if err != nil {
		t.Fatalf("ListStrandedForNodes: %v", err)
	}

	for _, id := range nodeIDs {
		all, err := ListForNode(db, id)
		if err != nil {
			t.Fatalf("ListForNode(%d): %v", id, err)
		}
		var wantActive, wantStranded []Bucket
		for _, b := range all {
			if b.State == StateActive {
				wantActive = append(wantActive, b)
			} else {
				wantStranded = append(wantStranded, b)
			}
		}
		if !sameSet(active[id], wantActive) {
			t.Errorf("active piles for node %d:\n batched = %+v\n per-node = %+v", id, active[id], wantActive)
		}
		if !sameSet(stranded[id], wantStranded) {
			t.Errorf("stranded piles for node %d:\n batched = %+v\n per-node = %+v", id, stranded[id], wantStranded)
		}
	}

	// A node with no piles must simply be absent, which reads the same as the
	// per-node form's empty slice at the call site.
	if got, ok := active[999]; ok {
		t.Errorf("node 999 has no piles but appears in the active map: %+v", got)
	}
}

// sameSet compares two pile lists regardless of order (the batched form orders
// by updated_at, which ties within one second).
func sameSet(a, b []Bucket) bool {
	if len(a) != len(b) {
		return false
	}
	byID := make(map[int64]Bucket, len(a))
	for _, x := range a {
		byID[x.ID] = x
	}
	for _, y := range b {
		if x, ok := byID[y.ID]; !ok || !reflect.DeepEqual(x, y) {
			return false
		}
	}
	return true
}

// A repeated node id must not duplicate that node's buckets. A station can list
// the same node twice — a changeover participant adopted as a child tile — so
// the id slice handed to the batch is not guaranteed to be unique.
func TestListForNodes_DeduplicatesNodeIDs(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	if _, err := Capture(db, 100, "P-500", 60); err != nil {
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
