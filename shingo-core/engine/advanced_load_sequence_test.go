//go:build docker

package engine

import (
	"errors"
	"strings"
	"testing"

	"shingocore/fleet"
	"shingocore/fleet/simulator"
	"shingocore/store/payloads"
)

// fakeBinCheckBackend wraps the simulator and adds CheckLocationTasks so it
// satisfies fleet.BinTaskChecker — the plain simulator does not, which is
// exactly what the "unverified" degrade-path test needs. tasks maps a location
// to the binTask keys configured there; a location absent from the map comes
// back Exists=false (not in the scene).
type fakeBinCheckBackend struct {
	*simulator.SimulatorBackend
	tasks map[string][]string
	err   error
}

func (f *fakeBinCheckBackend) CheckLocationTasks(locations []string) ([]fleet.LocationTasks, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]fleet.LocationTasks, 0, len(locations))
	for _, loc := range locations {
		names, ok := f.tasks[loc]
		out = append(out, fleet.LocationTasks{
			Location:  loc,
			Exists:    ok,
			Valid:     ok,
			TaskNames: names,
		})
	}
	return out, nil
}

// assignedCartPayload creates a fresh payload assigned to the standard storage
// node (STORAGE-A1), so the payload's load-location set is exactly that one node.
func assignedCartPayload(t *testing.T, db interface {
	CreatePayload(*payloads.Payload) error
	AssignPayloadToNode(nodeID, payloadID int64) error
}, storageNodeID int64) *payloads.Payload {
	t.Helper()
	p := &payloads.Payload{Code: "CART-TEST", Description: "child cart test payload"}
	if err := db.CreatePayload(p); err != nil {
		t.Fatalf("create payload: %v", err)
	}
	if err := db.AssignPayloadToNode(storageNodeID, p.ID); err != nil {
		t.Fatalf("assign payload to node: %v", err)
	}
	return p
}

const childCartSeq = "Child cart interlock" // seeded by migration v50

// TestValidateAdvancedLoadSequence_Empty: an empty sequence name is the normal-
// load default — always verified, no RDS work.
func TestValidateAdvancedLoadSequence_Empty(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())

	check, err := eng.ValidateAdvancedLoadSequence(0, "")
	if err != nil {
		t.Fatalf("empty sequence must not error: %v", err)
	}
	if !check.Verified {
		t.Errorf("empty sequence should be Verified, got %+v", check)
	}
}

// TestValidateAdvancedLoadSequence_UnknownName: the named red test — an unknown
// sequence name is rejected (non-nil error) and the message names the sequence.
func TestValidateAdvancedLoadSequence_UnknownName(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, _, _ := setupTestData(t, db)
	p := assignedCartPayload(t, db, storageNode.ID)

	full := map[string][]string{"STORAGE-A1": {"Go_AP1", "Spin_90", "load", "Spin_inverse_90"}}
	eng := newTestEngine(t, db, &fakeBinCheckBackend{SimulatorBackend: simulator.New(), tasks: full})

	check, err := eng.ValidateAdvancedLoadSequence(p.ID, "No Such Sequence")
	if err == nil {
		t.Fatal("unknown sequence name must be rejected")
	}
	if !strings.Contains(err.Error(), "No Such Sequence") {
		t.Errorf("error %q should name the unknown sequence", err.Error())
	}
	if check == nil || check.Verified {
		t.Errorf("unknown sequence must not verify, got %+v", check)
	}
}

// TestValidateAdvancedLoadSequence_MissingKey: RDS answered and a key is absent
// at a real location → hard reject, message names the location AND the key.
func TestValidateAdvancedLoadSequence_MissingKey(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, _, _ := setupTestData(t, db)
	p := assignedCartPayload(t, db, storageNode.ID)

	// STORAGE-A1 exists but has only two of the four keys.
	partial := map[string][]string{"STORAGE-A1": {"Go_AP1", "Spin_90"}}
	eng := newTestEngine(t, db, &fakeBinCheckBackend{SimulatorBackend: simulator.New(), tasks: partial})

	check, err := eng.ValidateAdvancedLoadSequence(p.ID, childCartSeq)
	if err == nil {
		t.Fatal("a missing key at a real location must reject the save")
	}
	if !strings.Contains(err.Error(), "STORAGE-A1") {
		t.Errorf("error %q should name the location", err.Error())
	}
	if !strings.Contains(err.Error(), "load") || !strings.Contains(err.Error(), "Spin_inverse_90") {
		t.Errorf("error %q should name the missing keys (load, Spin_inverse_90)", err.Error())
	}
	if check == nil || len(check.Missing) == 0 {
		t.Errorf("expected Missing populated, got %+v", check)
	}
}

// TestValidateAdvancedLoadSequence_AllPresent: every key present at every load
// location → Verified, no error, no warnings.
func TestValidateAdvancedLoadSequence_AllPresent(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, _, _ := setupTestData(t, db)
	p := assignedCartPayload(t, db, storageNode.ID)

	full := map[string][]string{"STORAGE-A1": {"Go_AP1", "Spin_90", "load", "Spin_inverse_90", "extra"}}
	eng := newTestEngine(t, db, &fakeBinCheckBackend{SimulatorBackend: simulator.New(), tasks: full})

	check, err := eng.ValidateAdvancedLoadSequence(p.ID, childCartSeq)
	if err != nil {
		t.Fatalf("all keys present must verify: %v", err)
	}
	if !check.Verified {
		t.Errorf("expected Verified, got %+v", check)
	}
}

// TestValidateAdvancedLoadSequence_NoCheckerDegrades: the simulator exposes no
// binTask check → the save is allowed with a warning (unverified), never an
// error. This is the "config not hostage to vendor uptime / sim dev" rule.
func TestValidateAdvancedLoadSequence_NoCheckerDegrades(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, _, _ := setupTestData(t, db)
	p := assignedCartPayload(t, db, storageNode.ID)

	eng := newTestEngine(t, db, simulator.New()) // plain sim: not a BinTaskChecker

	check, err := eng.ValidateAdvancedLoadSequence(p.ID, childCartSeq)
	if err != nil {
		t.Fatalf("no checker must not reject: %v", err)
	}
	if check.Verified {
		t.Errorf("expected unverified (not Verified) with a warning, got %+v", check)
	}
	if len(check.Warnings) == 0 {
		t.Errorf("expected a warning explaining why it couldn't verify, got %+v", check)
	}
}

// TestValidateAdvancedLoadSequence_RDSErrorDegrades: RDS is reachable-shaped but
// errors → warn-and-allow (not reject), per the split rule.
func TestValidateAdvancedLoadSequence_RDSErrorDegrades(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, _, _ := setupTestData(t, db)
	p := assignedCartPayload(t, db, storageNode.ID)

	eng := newTestEngine(t, db, &fakeBinCheckBackend{
		SimulatorBackend: simulator.New(),
		err:              errors.New("connection refused"),
	})

	check, err := eng.ValidateAdvancedLoadSequence(p.ID, childCartSeq)
	if err != nil {
		t.Fatalf("an RDS error must degrade to a warning, not reject: %v", err)
	}
	if len(check.Warnings) == 0 || check.Verified {
		t.Errorf("expected unverified with a warning, got %+v", check)
	}
}
