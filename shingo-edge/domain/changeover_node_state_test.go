package domain

import "testing"

// changeover_node_state_test.go — C(ii): the two park states against the
// completion-gate predicate. awaiting_material must hold the gate (no
// carve-out — Amendment 1 struck it), abandoned must open it, for every
// situation the task can carry.

func TestNodeTaskState_AwaitingMaterialIsNeverTerminal(t *testing.T) {
	t.Parallel()
	for _, situation := range []string{"swap", "evacuate", "drop", "add", "unchanged"} {
		if NodeTaskAwaitingMaterial.IsTerminal(situation) {
			t.Errorf("awaiting_material terminal for %q — the park would stop blocking the cutover gate", situation)
		}
	}
}

func TestNodeTaskState_AbandonedIsAlwaysTerminal(t *testing.T) {
	t.Parallel()
	for _, situation := range []string{"swap", "evacuate", "drop", "add"} {
		if !NodeTaskAbandoned.IsTerminal(situation) {
			t.Errorf("abandoned NOT terminal for %q — an operator-settled task must let the changeover complete", situation)
		}
	}
}
