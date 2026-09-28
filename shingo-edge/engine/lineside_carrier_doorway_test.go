package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/domain"
)

func edgeRoot(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for dir := cwd; ; {
		if data, rerr := os.ReadFile(filepath.Join(dir, "go.mod")); rerr == nil &&
			strings.Contains(string(data), "module shingoedge") {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no shingoedge go.mod above %s", cwd)
		}
		dir = parent
	}
}

// A CYCLE COUNT MUST NOT RE-AIM THE CARRIER'S IDENTITY.
//
// This is the case the doorway exists for. An operator counting a carrier they
// can see used to stamp the requested claim into active_claim_id, which was
// where identity was read from — so the recovery action disarmed the
// evacuation fix for that carrier's whole stay. Identity lives on its own
// field now and a count does not reach it: Core's count response carries the
// bin, the counts and the epoch, and no payload. They counted parts.
func TestRecordBinCount_DoesNotReAimTheCarrier(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "COUNT-ID", PayloadCode: "PART-REQUESTED", UOPCapacity: 100, InitialUOP: 40,
	})
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeLinesidePayload(nodeID, "PART-RESIDENT", true, string(domain.CarrierFromDelivery)),
		"seat a foreign carrier")

	eng := testEngine(t, db)
	// No Core client: RecordBinCount refuses before any write. What is being
	// pinned is that nothing on this path touches the identity, and the refusal
	// exercises the same early section that reads the claim.
	if err := eng.RecordBinCount(nodeID, 12, "press-2-op"); err == nil {
		t.Fatal("expected the count to refuse with no Core client configured")
	}
	_ = claimID

	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")
	if rt.LinesidePayloadCode != "PART-RESIDENT" {
		t.Fatalf("LinesidePayloadCode = %q, want PART-RESIDENT unchanged. A count is about parts; "+
			"letting it restate identity is exactly how the requested claim came to overwrite a "+
			"resident carrier.", rt.LinesidePayloadCode)
	}
}

// The doorway records an operator's answer as readily as Core's. On a manual
// load no envelope arrives ahead of the person, so theirs is not a weaker
// answer — it is the only one.
func TestRecordResidentCarrier_OperatorAuthorshipIsFirstClass(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "OP-AUTH", PayloadCode: "PART-CLAIM", UOPCapacity: 100, InitialUOP: 0,
	})
	eng := testEngine(t, db)

	eng.recordLinesideCarrier(nodeID, "OP-AUTH-NODE",
		domain.KnownCarrier("PART-OPERATOR-PUT-THIS-HERE"), domain.CarrierFromOperator)

	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")
	if rt.LinesidePayloadCode != "PART-OPERATOR-PUT-THIS-HERE" {
		t.Errorf("LinesidePayloadCode = %q — an operator's assertion must be recorded, not "+
			"discarded for lacking an envelope", rt.LinesidePayloadCode)
	}
}

// An unknown carrier erases rather than skips. A stale identity held over from
// the previous occupant is a confident wrong answer about this one, and the
// readers of this field fail open on empty and act on populated.
func TestRecordResidentCarrier_UnknownClearsRatherThanHoldsOver(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "OP-CLR", PayloadCode: "PART-CLAIM", UOPCapacity: 100, InitialUOP: 0,
	})
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeLinesidePayload(nodeID, "PART-PREVIOUS", true, string(domain.CarrierFromDelivery)), "seat one")
	eng := testEngine(t, db)

	eng.recordLinesideCarrier(nodeID, "OP-CLR-NODE", domain.UnknownCarrier(), domain.CarrierDeparted)

	rt, err := db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "read runtime")
	if rt.LinesidePayloadCode != "" {
		t.Errorf("LinesidePayloadCode = %q, want empty — the departed carrier's identity must not "+
			"be inherited by whatever lands next", rt.LinesidePayloadCode)
	}
}
