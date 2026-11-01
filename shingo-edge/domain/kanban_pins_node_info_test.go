package domain

import (
	"encoding/json"
	"strings"
	"testing"

	"shingo/protocol"
)

// TestKanbanPin_NodeInfoKanbanOnTheEdge pins how the Edge reads Core's node
// list: the wire key, the one set it feeds, and the one refusal
// that set decides. An absent key is false, so a Core that does not send it refuses
// nothing.
func TestKanbanPin_NodeInfoKanbanOnTheEdge(t *testing.T) {
	t.Parallel()
	var nodes []protocol.NodeInfo
	if err := json.Unmarshal([]byte(`[
		{"name":"KEPT","node_type":"NGRP","kanban":true},
		{"name":"PLAIN","node_type":"NGRP"}]`), &nodes); err != nil {
		t.Fatalf("decode: %v", err)
	}
	known := map[string]protocol.NodeInfo{}
	for _, n := range nodes {
		known[n.Name] = n
	}
	kinds := CoreNodeKindsOf(known)
	for name, want := range map[string]bool{"KEPT": true, "Parent.KEPT": true, "PLAIN": false, "UNKNOWN": false} {
		if got := kinds.IsKanban(name); got != want {
			t.Errorf("IsKanban(%q) = %v, want %v", name, got, want)
		}
	}
	if kept := CoreNodeKindsOf(nil); kept.IsKanban("KEPT") {
		t.Error("no node list heard yet must refuse nothing")
	}

	for _, tc := range []struct {
		name    string
		role    protocol.ClaimRole
		source  string
		refused bool
	}{
		{"a consume claim's source in the group", protocol.ClaimRoleConsume, "KEPT", true},
		{"a consume claim's source elsewhere", protocol.ClaimRoleConsume, "PLAIN", false},
		{"a produce claim's source in the group", protocol.ClaimRoleProduce, "KEPT", false},
	} {
		var msgs []string
		validateLegKinds(NodeClaimInput{Role: tc.role, InboundSource: tc.source}, kinds, map[string]bool{},
			func(field, msg string) { msgs = append(msgs, field+": "+msg) })
		refused := len(msgs) == 1 && strings.HasPrefix(msgs[0], "inbound_source: "+protocol.MsgFullsFromAKanban)
		if refused != tc.refused || (!tc.refused && len(msgs) != 0) {
			t.Errorf("%s: findings %v, refused want %v", tc.name, msgs, tc.refused)
		}
	}
}
