//go:build docker

package payloads_test

import (
	"reflect"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/domain"
	"shingocore/internal/testdb"
	"shingocore/store/payloads"
)

// These tests OWN the v50 migration (authored elsewhere, verified here against
// the F4c C1 spec): the advanced_load_sequence column defaults to empty, the
// load_sequences registry exists, and it is seeded with the child-cart sequence
// in the evidence-doc order.

// TestPayload_AdvancedLoadSequenceDefaultsEmpty pins the "empty = normal load"
// contract at the storage layer: a payload created without the field round-trips
// as an empty string (never NULL — the column is NOT NULL DEFAULT ”).
func TestPayload_AdvancedLoadSequenceDefaultsEmpty(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t).DB

	p := &payloads.Payload{Code: "PC-ALS-1", Description: "default check"}
	testutil.MustNoErr(t, payloads.Create(db, p), "create")

	got, err := payloads.Get(db, p.ID)
	testutil.MustNoErr(t, err, "get")
	if got.AdvancedLoadSequence != "" {
		t.Errorf("AdvancedLoadSequence = %q, want empty", got.AdvancedLoadSequence)
	}
}

// TestPayload_AdvancedLoadSequenceRoundTrips pins that a set value persists
// through Create/Update/Get.
func TestPayload_AdvancedLoadSequenceRoundTrips(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t).DB

	p := &payloads.Payload{Code: "PC-ALS-2", AdvancedLoadSequence: "Child cart interlock"}
	testutil.MustNoErr(t, payloads.Create(db, p), "create")
	got, err := payloads.Get(db, p.ID)
	testutil.MustNoErr(t, err, "get after create")
	if got.AdvancedLoadSequence != "Child cart interlock" {
		t.Fatalf("after create = %q, want Child cart interlock", got.AdvancedLoadSequence)
	}

	got.AdvancedLoadSequence = ""
	testutil.MustNoErr(t, payloads.Update(db, got), "update to empty")
	got2, err := payloads.Get(db, p.ID)
	testutil.MustNoErr(t, err, "get after update")
	if got2.AdvancedLoadSequence != "" {
		t.Errorf("after clearing = %q, want empty", got2.AdvancedLoadSequence)
	}
}

// TestLoadSequences_Seeded pins the v50 seed: the registry has "Child cart
// interlock" mapped to the four evidence-doc task names, in order.
func TestLoadSequences_Seeded(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t).DB

	seq, err := payloads.GetLoadSequence(db, "Child cart interlock")
	testutil.MustNoErr(t, err, "get seeded sequence")
	if seq == nil {
		t.Fatal("seeded sequence 'Child cart interlock' missing")
	}
	want := []string{"Go_AP1", "Spin_90", "load", "Spin_inverse_90"}
	if !reflect.DeepEqual(seq.TaskNames, want) {
		t.Errorf("TaskNames = %v, want %v (evidence-doc order)", seq.TaskNames, want)
	}

	names, err := payloads.ListLoadSequenceNames(db)
	testutil.MustNoErr(t, err, "list names")
	var found bool
	for _, n := range names {
		if n == "Child cart interlock" {
			found = true
		}
	}
	if !found {
		t.Errorf("ListLoadSequenceNames = %v, want it to include the seeded sequence", names)
	}
}

// TestGetLoadSequence_Unknown pins that an unregistered name is (nil, nil) — a
// "no such sequence" signal, not an error — so validation can reject it cleanly.
func TestGetLoadSequence_Unknown(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t).DB

	seq, err := payloads.GetLoadSequence(db, "Not A Real Sequence")
	testutil.MustNoErr(t, err, "get unknown")
	if seq != nil {
		t.Errorf("unknown sequence = %+v, want nil", seq)
	}
}

// Compile-time guard: the domain alias carries the new field (a positional
// regression would surface here rather than as a silent scan mismatch).
var _ = domain.Payload{AdvancedLoadSequence: ""}
