package domain

import (
	"strings"
	"testing"
)

// loader_zero_payload_test.go — what "a shared_window loader with no payloads"
// means, for each role.
//
// A shared window's payload set is what it can be offered: the unloader sweep
// offers PayloadSet() (so an empty set offers nothing and reads nothing —
// engine TestUnloaderSweep_ZeroPayloads_ZeroReads), LoaderForPayload asks
// ServesPayload, and the board lists LoadablePayloadCodesAt.

// TestSharedWindowZeroPayloads_ByRole: the constructor's answer for each role.
// A loader stages empties for a payload, so produce still needs one. An
// unloader may declare none (the stage-2 window of a two-stage unloader), and
// then serves no payload at all — not even the blank one.
func TestSharedWindowZeroPayloads_ByRole(t *testing.T) {
	t.Parallel()
	windows := []Window{{Node: "UNL-W1"}}

	l, err := NewSharedWindowLoader("loader:ZP-produce", "ZP", RoleProduce, ReplenishmentOperator, windows, nil)
	if err == nil {
		t.Errorf("produce shared_window with zero payloads built %+v; want the constructor to refuse it", l)
	} else if !strings.Contains(err.Error(), "shared_window needs at least one payload") {
		t.Errorf("produce zero-payload error = %q, want the at-least-one-payload refusal", err)
	}

	u, err := NewSharedWindowLoader("loader:ZP-consume", "ZP", RoleConsume, ReplenishmentOperator, windows, nil)
	if err != nil {
		t.Fatalf("consume shared_window with zero payloads: %v; want it built", err)
	}
	if n := len(u.PayloadSet()); n != 0 {
		t.Errorf("PayloadSet has %d entries, want 0", n)
	}
	if n := len(u.LoadablePayloadCodesAt("UNL-W1")); n != 0 {
		t.Errorf("LoadablePayloadCodesAt has %d entries, want 0", n)
	}
	for _, p := range []PayloadCode{"", "PART-A"} {
		if u.ServesPayload(p) {
			t.Errorf("ServesPayload(%q) = true, want false", p)
		}
	}
}

// TestSharedWindow_BlankPayloadReads: how a shared loader answers the blank
// payload. ServesPayload("") is false (the set never holds ""), while
// ReservationTarget treats "" as the payload-agnostic stage and still targets
// the windows.
func TestSharedWindow_BlankPayloadReads(t *testing.T) {
	t.Parallel()
	l, err := NewSharedWindowLoader("loader:BP", "BP", RoleConsume, ReplenishmentOperator,
		[]Window{{Node: "UNL-W1"}}, []PayloadCode{"PART-A"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if l.ServesPayload("") {
		t.Error(`ServesPayload("") = true, want false`)
	}
	if nodes, budget := l.ReservationTarget("", "", false); len(nodes) != 1 || budget != 1 {
		t.Errorf(`ReservationTarget("", "") = (%v, %d), want one node, budget 1`, nodes, budget)
	}
	if nodes, _ := l.ReservationTarget("", "PART-Z", false); len(nodes) != 0 {
		t.Errorf("ReservationTarget for an unserved payload = %v, want none", nodes)
	}
}
