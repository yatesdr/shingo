// changeover_gate_doors_test.go — the changeover gates at every door, in one
// table: the request doors (REQUEST's requestNodeFromClaim and the produce
// door's produceRequest) and the sweep (CanAcceptOrders), all asking the one
// gate (changeoverHolds). Each row is a position under an open changeover
// A -> B and what each door answers there. The re-ask under the cell's lock is
// TestGuards_TheReaskSeesAChangeoverArmedMeanwhile; A5 is its own guard and
// its own pins; the common positions' supply through a changeover is
// TestPinChangeover_ACommonPositionIsResuppliedWhileOpen.
package engine

import (
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// gateAt is one door's changeover gate at node for claim: whether it refuses,
// and the reason it gives. The request doors ask it with the claim in hand,
// the sweep with none.
func gateAt(eng *Engine, door string, node *processes.Node, claim *processes.NodeClaim) (bool, string) {
	if door == "sweep" {
		ok, why := eng.CanAcceptOrders(node.ID)
		return !ok, why
	}
	if err := eng.guardChangeover(nil, node, claim); err != nil {
		return true, err.Error()
	}
	return false, ""
}

// gateDoorRig is guardCountRig with three more positions — a loader the
// process shares (A claims it manual_swap), an indexed-over position (no claim,
// a participant only), a same-part position the changeover evacuates — and the
// changeover A -> B open over them all.
func gateDoorRig(t *testing.T) (eng *Engine, pid, a, b int64, nodes map[string]*processes.Node) {
	t.Helper()
	eng, _, pid, a, b, _ = guardCountRig(t)
	_, err := eng.db.CreateProcessNode(processes.NodeInput{ProcessID: pid, CoreNodeName: "N-LOADER", Code: "N-LOADER", Name: "N-LOADER", Enabled: true})
	testutil.MustNoErr(t, err, "the loader")
	_, err = upsertClaimRetiredMode(eng.db, processes.NodeClaimInput{StyleID: a, CoreNodeName: "N-LOADER", Role: "consume",
		SwapMode: "manual_swap", PayloadCode: "EMPTY-TOTE", UOPCapacity: 10, InboundSource: "SRC"})
	testutil.MustNoErr(t, err, "the loader's claim")
	_, err = eng.db.CreateProcessNode(processes.NodeInput{ProcessID: pid, CoreNodeName: "N-INDEXED", Code: "N-INDEXED", Name: "N-INDEXED", Enabled: true})
	testutil.MustNoErr(t, err, "the indexed-over position")
	cartNode(t, eng, pid, a, "N-EVAC", "PART-E", "PART-E", 9765)
	_, err = upsertClaimRetiredMode(eng.db, processes.NodeClaimInput{StyleID: b, CoreNodeName: "N-EVAC", Role: "consume",
		SwapMode: "simple", PayloadCode: "PART-E", UOPCapacity: 100, InboundSource: "SRC"})
	testutil.MustNoErr(t, err, "B's claim at N-EVAC")
	ns, err := eng.db.ListProcessNodesByProcess(pid)
	testutil.MustNoErr(t, err, "nodes")
	tasks := map[string]string{"N-COMMON": string(SituationUnchanged), "N-MIRROR": string(SituationSwap), "N-EVAC": string(SituationEvacuate)}
	var in []processes.NodeTaskInput
	var parts []domain.ParticipantInput
	for node, situation := range tasks {
		in = append(in, processes.NodeTaskInput{ProcessID: pid, CoreNodeName: node, Situation: situation, State: "swap_required"})
		parts = append(parts, domain.ParticipantInput{CoreNodeName: node, Role: domain.ParticipantRoleTask})
	}
	parts = append(parts, domain.ParticipantInput{CoreNodeName: "N-INDEXED", Role: domain.ParticipantRoleIndexedOver})
	_, err = eng.changeoverService.Create(pid, &a, b, "test", "", nil, in, parts, ns)
	testutil.MustNoErr(t, err, "the changeover A -> B")
	nodes = map[string]*processes.Node{}
	for i := range ns {
		nodes[ns[i].CoreNodeName] = &ns[i]
	}
	return eng, pid, a, b, nodes
}

// door is one door's expected answer: refused, and a word its reason holds.
type door struct {
	refused bool
	says    string
}

// TestPinGate_EveryDoorAnswersAsOne: under an open changeover A -> B —
//
//   - the loader the process shares is admitted at every door (the
//     Springfield regression);
//   - the indexed-over position, which owns no task and no claim, is refused
//     at every door (the owner's ruling: REQUEST refuses it too; before the
//     fold the request doors asked nothing of a position with no claim);
//   - the incoming style's request at the position whose part changes (a
//     keep-staged refill for B) is admitted by the request doors; the sweep,
//     which asks no claim, refuses the participant;
//   - the common position (the same part under both, its task unchanged) is
//     admitted at every door;
//   - the same-part position the changeover evacuates, and the outgoing
//     style's request at the position whose part changes, are refused at
//     every door.
func TestPinGate_EveryDoorAnswersAsOne(t *testing.T) {
	t.Parallel()
	eng, _, a, b, nodes := gateDoorRig(t)
	claim := func(style int64, node string) *processes.NodeClaim {
		c, err := eng.db.GetStyleNodeClaimByNode(style, node)
		testutil.MustNoErr(t, err, "the claim at "+node)
		return c
	}
	for _, row := range []struct {
		name    string
		node    string
		claim   *processes.NodeClaim
		request door // REQUEST and the produce door: the same claim-keyed gate
		sweep   door
	}{
		{name: "the loader the process shares", node: "N-LOADER", claim: claim(a, "N-LOADER"),
			request: door{}, sweep: door{}},
		{name: "the indexed-over position", node: "N-INDEXED", claim: nil,
			request: door{refused: true, says: "indexed-over"}, sweep: door{refused: true, says: "indexed-over"}},
		{name: "the incoming style's refill where the part changes", node: "N-MIRROR", claim: claim(b, "N-MIRROR"),
			request: door{}, sweep: door{refused: true, says: "changeover in progress"}},
		{name: "the common position", node: "N-COMMON", claim: claim(a, "N-COMMON"),
			request: door{}, sweep: door{}},
		{name: "the same-part position the changeover evacuates", node: "N-EVAC", claim: claim(a, "N-EVAC"),
			request: door{refused: true, says: "armed"}, sweep: door{refused: true, says: "changeover in progress"}},
		{name: "the outgoing style's request where the part changes", node: "N-MIRROR", claim: claim(a, "N-MIRROR"),
			request: door{refused: true, says: "armed"}, sweep: door{refused: true, says: "changeover in progress"}},
	} {
		node := nodes[row.node]
		for _, d := range []struct {
			name string
			want door
		}{{"REQUEST", row.request}, {"the produce door", row.request}, {"sweep", row.sweep}} {
			refused, why := gateAt(eng, d.name, node, row.claim)
			if refused != d.want.refused || (d.want.says != "" && !strings.Contains(why, d.want.says)) {
				t.Errorf("%s at %s: refused %t (%q), want refused %t saying %q", row.name, d.name, refused, why, d.want.refused, d.want.says)
			}
		}
	}
}

// TestPinGate_AFailedParticipantLookupHoldsTheSweep: with the changeover open
// and its participants unreadable, the sweep refuses, naming the lookup: it
// fails closed, as a false "available" is the two-bins case.
func TestPinGate_AFailedParticipantLookupHoldsTheSweep(t *testing.T) {
	t.Parallel()
	eng, _, _, _, nodes := gateDoorRig(t)
	_, err := eng.db.Exec(`DROP TABLE changeover_participants`)
	testutil.MustNoErr(t, err, "the participants unreadable")
	if refused, why := gateAt(eng, "sweep", nodes["N-COMMON"], nil); !refused || why != "changeover in progress (participant lookup failed)" {
		t.Errorf("the sweep at the common position: refused %t (%q), want the participant lookup's refusal", refused, why)
	}
}
