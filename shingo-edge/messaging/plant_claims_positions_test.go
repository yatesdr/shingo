package messaging

import (
	"encoding/json"
	"reflect"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// Lane 0 item 6 (BRIEF-unification-D1; C2c DECISIONS D9, D30; DESIGN-H §4 S1).
// A loader window, a loader home or an FG drain is an Edge process_node with no
// style_node_claims row (Core owns loader configuration; the Edge serves the
// window through Loader.SynthClaim), so the claims alone never told Core which
// station binds there. The report's positions carry the process's live
// process_nodes that no published claim names; Core mirrors them and the
// landing's address read finds the station.

// reportKeys builds one process's report and returns it decoded twice: as the
// typed report and as a key map, so a test can see a key's absence on the wire.
func reportKeys(t *testing.T, p *PlantClaimsPublisher, pid int64) map[string]json.RawMessage {
	t.Helper()
	proc, err := processes.Get(p.db.DB, pid)
	testutil.MustNoErr(t, err, "get process")
	data, err := p.buildProcess(*proc)
	testutil.MustNoErr(t, err, "build")
	var env protocol.Envelope
	testutil.MustNoErr(t, json.Unmarshal(data, &env), "envelope")
	var d protocol.Data
	testutil.MustNoErr(t, env.DecodePayload(&d), "data")
	var keys map[string]json.RawMessage
	testutil.MustNoErr(t, json.Unmarshal(d.Body, &keys), "report keys")
	return keys
}

func addProcessNode(t *testing.T, p *PlantClaimsPublisher, pid int64, core string) int64 {
	t.Helper()
	id, err := p.db.CreateProcessNode(processes.NodeInput{
		ProcessID: pid, CoreNodeName: core, Code: core, Name: core, Enabled: true,
	})
	testutil.MustNoErr(t, err, "create process node "+core)
	return id
}

// TestPlantClaimsPublisher_LoaderPositionsOnTheWire: the loader process's
// windows (process_nodes, no claim) are published as positions, sorted; a node
// a claim names (its own or a paired position) is not; a retired node is not.
func TestPlantClaimsPublisher_LoaderPositionsOnTheWire(t *testing.T) {
	t.Parallel()
	db, counter := countingPublisherDB(t)
	p := NewPlantClaimsPublisher(db, "plant-a.line-1", 0)

	pid, ids := seedClaimsProcess(t, db, "LDR", 1, 1) // one claim, on N-0
	if _, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
		StyleID: ids[0], CoreNodeName: "PRS-A", Role: protocol.ClaimRoleConsume,
		SwapMode: protocol.SwapModeSequential, PayloadCode: "P-A", UOPCapacity: 10,
		PairedCoreNode: "PRS-B", InboundSource: "IN", OutboundDestination: "OUT",
	}); err != nil {
		t.Fatalf("paired claim: %v", err)
	}
	addProcessNode(t, p, pid, "N-0")   // claimed
	addProcessNode(t, p, pid, "PRS-B") // a paired position of a claim
	addProcessNode(t, p, pid, "PLK-W2")
	addProcessNode(t, p, pid, "PLK-W1")
	gone := addProcessNode(t, p, pid, "PLK-OLD")
	testutil.MustNoErr(t, db.DeleteProcessNode(gone), "retire PLK-OLD")

	counter.Reset()
	keys := reportKeys(t, p, pid)
	reads := counter.Count()

	var got []string
	if raw, ok := keys["positions"]; ok {
		testutil.MustNoErr(t, json.Unmarshal(raw, &got), "positions")
	}
	if want := []string{"PLK-W1", "PLK-W2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("positions = %v, want %v: the windows no claim names, sorted; not a claimed node, "+
			"not a paired position, not a retired node", got, want)
	}

	// The read count of a build is the two reads it always made: the positions
	// ride the styles read (a third read would be one more statement per
	// process on every heartbeat's reconcile, on the Pi).
	plain, _ := seedClaimsProcess(t, db, "PLAIN", 1, 1)
	counter.Reset()
	_ = reportKeys(t, p, plain)
	if reads != counter.Count() {
		t.Errorf("a build with positions read %d statements, one without %d: the positions must ride a read the build already makes",
			reads, counter.Count())
	}
}

// TestPlantClaimsPublisher_NoPositionsNoKey: a process whose every node is
// claimed publishes no positions key, so its report and digest are what they
// were before the field existed (no republish storm on deploy).
func TestPlantClaimsPublisher_NoPositionsNoKey(t *testing.T) {
	t.Parallel()
	db, _ := countingPublisherDB(t)
	p := NewPlantClaimsPublisher(db, "plant-a.line-1", 0)

	pid, _ := seedClaimsProcess(t, db, "CLAIMED", 1, 2) // claims on N-0, N-1
	addProcessNode(t, p, pid, "N-0")
	addProcessNode(t, p, pid, "N-1")

	keys := reportKeys(t, p, pid)
	if _, ok := keys["positions"]; ok {
		t.Fatalf("a process whose every node is claimed published positions %s", keys["positions"])
	}
}
