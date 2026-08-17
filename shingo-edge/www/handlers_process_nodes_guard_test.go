package www

import (
	"testing"

	"shingo/protocol"
)

// handlers_process_nodes_guard_test.go — a minted core_node_name is a claim
// about Core's plant, and the claim is now checked.
//
// process_nodes.core_node_name is free text on this side: an operator types it
// into a config form and Edge stores it. A name that matches nothing on Core is
// accepted happily and discovered much later — as an order that will not
// dispatch, a UOP adjustment that lands nowhere, or a claim configured against a
// slot that does not exist. The link between the two node families is
// GUARDED, NOT CONSTRAINED, and it cannot be constrained: they live in different
// databases in different services.
//
// Core's node set already arrives roughly every two minutes and Edge keeps it in
// engine.CoreNodes(), where it has been display-only. Reading it at the mint
// sites is the entire available remedy.

// TestMintGuard_RefusesANameCoreDoesNotHave is the guard.
func TestMintGuard_RefusesANameCoreDoesNotHave(t *testing.T) {
	h, _ := newTestHandlers(t)
	h.engine.(*stubEngine).core = map[string]protocol.NodeInfo{
		"ALN_001": {},
		"SM_A.W1": {},
	}

	msg, unknown := h.coreNodeNameIsUnknown("ALN_009")
	if !unknown {
		t.Fatal("a name Core does not have was accepted. It is stored happily and only surfaces " +
			"later as an order that will not dispatch or a count written nowhere")
	}
	if msg == "" {
		t.Error("the refusal carries no message; a failure message names what to change")
	}
}

// TestMintGuard_AcceptsAKnownName is the regression half: the guard must not
// refuse the ordinary case, which is every legitimate configuration write.
func TestMintGuard_AcceptsAKnownName(t *testing.T) {
	h, _ := newTestHandlers(t)
	h.engine.(*stubEngine).core = map[string]protocol.NodeInfo{"ALN_001": {}}

	if _, unknown := h.coreNodeNameIsUnknown("ALN_001"); unknown {
		t.Error("a node Core DOES have was refused — this guard sits on the configuration path " +
			"every process node is created through")
	}
}

// TestMintGuard_AcceptsAGroupChildByItsBareName pins the fallback.
//
// Core sends group children as "Group.CHILD" for display and the rest of Edge
// keys on the bare child name (see bareNodeName). Without this arm the guard
// would refuse every group-child node — which is most of a supermarket — and the
// refusal would look like a Core sync problem rather than a spelling rule.
func TestMintGuard_AcceptsAGroupChildByItsBareName(t *testing.T) {
	h, _ := newTestHandlers(t)
	h.engine.(*stubEngine).core = map[string]protocol.NodeInfo{"SM_A.W1": {}}

	if _, unknown := h.coreNodeNameIsUnknown("W1"); unknown {
		t.Error("a group child was refused by its bare name. Core sends 'Group.CHILD' for display " +
			"and the runtime keys on the child; refusing here would reject most of a supermarket")
	}
}

// TestMintGuard_EmptyCoreListDoesNotRefuse is the one that matters most, and it
// is the batch's standing rule applied to a guard: A CHECK MUST KNOW WHETHER IT
// HAD THE INPUT TO CHECK.
//
// An empty node set is not evidence that a name is wrong. It means Core has not
// been heard from — a fresh Edge, a restart, a Kafka gap — and that is exactly
// when somebody is most likely to be configuring nodes. Refusing on it would
// brick setup, and it would do so with a message blaming the operator's
// spelling for a wire problem.
//
// MUTATION (verified): drop the len(known) == 0 arm and this fails — every
// configuration write on an Edge that has not yet heard from Core is refused.
func TestMintGuard_EmptyCoreListDoesNotRefuse(t *testing.T) {
	h, _ := newTestHandlers(t)
	h.engine.(*stubEngine).core = map[string]protocol.NodeInfo{}

	if _, unknown := h.coreNodeNameIsUnknown("ANYTHING_AT_ALL"); unknown {
		t.Error("the guard refused while it had NOTHING to check against. An empty node set means " +
			"Core has not been heard from, not that the name is wrong — and absence of data must " +
			"never render as a finding")
	}
}
