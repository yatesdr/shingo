package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// Cloned from TestOrderUpdate_Fault_MixedVersion, the precedent for adding
// fields to this wire: additive, omitempty both directions, no migration.
//
// WITH ONE DIFFERENCE WORTH STATING, because it changes the deploy order. Every
// earlier addition travelled Core→Edge and degraded cosmetically — an Edge that
// had never heard of a fault clock still printed a sentence. This one travels
// Edge→Core and degrades MATERIALLY: an old Core ignores it and resolves the
// refill against the order's payload, which is the outgoing style's, which is
// the wrong carrier. Deploy Core first.
func TestComplexOrderStep_PayloadCode_MixedVersion(t *testing.T) {
	step := ComplexOrderStep{
		Action:      ActionPickup,
		Node:        "SYN_PRESS_EMPTIES",
		Empty:       true,
		PayloadCode: "PANEL-C",
	}
	buf, err := json.Marshal(step)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// New Core reads it.
	var got ComplexOrderStep
	if err := json.Unmarshal(buf, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.PayloadCode != "PANEL-C" {
		t.Errorf("round-trip lost PayloadCode: %+v", got)
	}
	if !got.Empty || got.Node != "SYN_PRESS_EMPTIES" {
		t.Errorf("round-trip disturbed the fields around it: %+v", got)
	}

	// Old Core — a decoder that never heard of the field — still reads the leg
	// it always understood, and does NOT error.
	var oldCore struct {
		Action string `json:"action"`
		Node   string `json:"node"`
		Empty  bool   `json:"empty"`
	}
	if err := json.Unmarshal(buf, &oldCore); err != nil {
		t.Fatalf("an old Core must ignore the field, not fail on it: %v", err)
	}
	if oldCore.Node != "SYN_PRESS_EMPTIES" || !oldCore.Empty {
		t.Errorf("old Core lost the leg it understood: %+v", oldCore)
	}
	// ...and this is the consequence to deploy around: with no per-step payload
	// it falls back to the order's, which on a changeover is the outgoing
	// style's. Nothing here can prevent that; naming it is the point.
}

// A step that says nothing about payload must SEND nothing, so the wire is
// unchanged for every leg that does not need this — the omitempty half of the
// compatibility claim.
func TestComplexOrderStep_PayloadCode_OmittedWhenUnset(t *testing.T) {
	buf, err := json.Marshal(ComplexOrderStep{Action: ActionDropoff, Node: "PLN_001"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(buf), "payload_code") {
		t.Errorf("an unset payload was serialised anyway: %s", buf)
	}
}
