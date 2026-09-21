package protocol

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"testing"
)

// plant_claim_legs_test.go — the plant.claims wire contract, pinned at the
// grain the fleet actually pays for.
//
// PlantClaim is the sourceability subset of an Edge NodeClaim, and the S0
// demand loop needs it to carry the claim's LEGS as well: where a node's
// inbound material comes from, where its outbound goes, and the paired
// positions of a two-robot cell. Those four fields are additive, which is a
// claim about three separate things and every one of them is checked here:
//
//  1. a claim that reports no legs encodes to the SAME BYTES it does today
//     (omitempty), so an Edge that never sets them costs the Pi nothing;
//  2. a message from an OLD Edge — one whose JSON simply has no such keys —
//     decodes into blanks without an error;
//  3. a message from a NEW Edge decoding into an OLD Core's struct — one that
//     has no such fields — drops the unknown keys without an error.
//
// (2) and (3) are the two halves of a rollout: Edge deploys before Core, so
// both directions exist in the field at the same time, and neither may be a
// decode failure.

// legacyPlantClaimKeys is the plant.claims claim key set as it shipped before
// the leg fields. Every key here is REQUIRED on the wire (none carries
// omitempty), so this is exactly what an unset claim encodes to.
var legacyPlantClaimKeys = []string{
	"allowed_payload_codes",
	"core_node_name",
	"payload_code",
	"reorder_point",
	"role",
	"swap_mode",
	"uop_capacity",
}

// oldCorePlantClaim is protocol.PlantClaim as an OLD CORE BINARY holds it —
// the seven fields above and nothing else. It is a copy on purpose: the point
// of the test that uses it is what a binary WITHOUT the new fields does with a
// payload that has them, and that binary cannot be simulated by the struct
// this package is currently compiling.
type oldCorePlantClaim struct {
	CoreNodeName        string    `json:"core_node_name"`
	Role                ClaimRole `json:"role"`
	SwapMode            SwapMode  `json:"swap_mode"`
	PayloadCode         string    `json:"payload_code"`
	AllowedPayloadCodes []string  `json:"allowed_payload_codes"`
	UOPCapacity         int       `json:"uop_capacity"`
	ReorderPoint        int       `json:"reorder_point"`
}

// jsonKeys returns the top-level object keys of an encoded value, sorted.
func jsonKeys(t *testing.T, b []byte) []string {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil {
		t.Fatalf("unmarshal into a key map: %v", err)
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestPlantClaim_ClaimReportingNoLegsEncodesToTheLegacyKeySet is the byte
// argument, stated as a key set so a failure names the field that broke it.
//
// THE EDGE IS A PI, and this feed is one message per process on every spec
// edit plus an hourly full snapshot per process. A required (non-omitempty)
// leg field would put four more keys on EVERY claim at EVERY plant, including
// the plants that will never configure one, which is a cost paid by the whole
// fleet for a feature a few cells use. omitempty is what makes the unset case
// free, and this test is what keeps it that way.
func TestPlantClaim_ClaimReportingNoLegsEncodesToTheLegacyKeySet(t *testing.T) {
	t.Parallel()
	c := PlantClaim{
		CoreNodeName:        "PLN_002",
		Role:                ClaimRoleConsume,
		SwapMode:            SwapModeSequential,
		PayloadCode:         "SYN-PART-A",
		AllowedPayloadCodes: []string{"SYN-PART-A"},
		UOPCapacity:         120,
		ReorderPoint:        30,
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := jsonKeys(t, b); !reflect.DeepEqual(got, legacyPlantClaimKeys) {
		t.Errorf("a claim reporting no legs encodes keys %v, want %v.\n"+
			"Every field added to PlantClaim must carry omitempty: this feed is one message "+
			"per process per spec edit plus an hourly snapshot, on a Pi, and a required field "+
			"bills every claim at every plant for a leg most of them never configure.",
			got, legacyPlantClaimKeys)
	}
}

// TestPlantClaim_OldEdgeReportDecodesToBlanks is the old-Edge-to-new-Core half
// of the rollout. The JSON below is what an Edge that has never heard of the
// leg fields sends; decoding it must succeed and must leave every field this
// Core knows about but the sender did not send at its zero value.
//
// Written as a whole-struct comparison against a literal rather than a
// field-by-field check, so a field added to PlantClaim without a wire default
// of "" shows up here as a diff instead of passing unexamined.
func TestPlantClaim_OldEdgeReportDecodesToBlanks(t *testing.T) {
	t.Parallel()
	const oldEdgeClaim = `{
		"core_node_name": "PLN_002",
		"role": "consume",
		"swap_mode": "sequential",
		"payload_code": "SYN-PART-A",
		"allowed_payload_codes": ["SYN-PART-A"],
		"uop_capacity": 120,
		"reorder_point": 30
	}`
	var got PlantClaim
	if err := json.Unmarshal([]byte(oldEdgeClaim), &got); err != nil {
		t.Fatalf("an old Edge's claim must decode without error: %v", err)
	}
	want := PlantClaim{
		CoreNodeName:        "PLN_002",
		Role:                ClaimRoleConsume,
		SwapMode:            SwapModeSequential,
		PayloadCode:         "SYN-PART-A",
		AllowedPayloadCodes: []string{"SYN-PART-A"},
		UOPCapacity:         120,
		ReorderPoint:        30,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("old Edge claim decoded to %+v, want %+v — an absent key must read as the "+
			"zero value (\"not reported\"), never as an error and never as a guess", got, want)
	}
}

// TestPlantClaim_NewEdgeReportIsAcceptedByAnOldCore is the other half, and the
// one that cannot be written against the current struct: it decodes a payload
// carrying the leg keys into oldCorePlantClaim, which is the struct the binary
// running at a plant that has not been upgraded yet actually holds.
//
// encoding/json drops an unknown key silently — there is no
// DisallowUnknownFields anywhere in this tree, and protocol/payloads.go states
// that as the additive-schema rule — so the old Core keeps mirroring the seven
// fields it knows and never sees the four it does not.
func TestPlantClaim_NewEdgeReportIsAcceptedByAnOldCore(t *testing.T) {
	t.Parallel()
	const newEdgeClaim = `{
		"core_node_name": "PLN_002",
		"role": "consume",
		"swap_mode": "sequential",
		"payload_code": "SYN-PART-A",
		"allowed_payload_codes": ["SYN-PART-A"],
		"uop_capacity": 120,
		"reorder_point": 30,
		"inbound_source": "SMN_SYN_A",
		"outbound_destination": "SMN_SYN_B",
		"paired_core_node": "PLN_003",
		"second_paired_core_node": "PLN_004"
	}`
	var got oldCorePlantClaim
	if err := json.Unmarshal([]byte(newEdgeClaim), &got); err != nil {
		t.Fatalf("a new Edge's claim must decode on an old Core without error: %v", err)
	}
	want := oldCorePlantClaim{
		CoreNodeName:        "PLN_002",
		Role:                ClaimRoleConsume,
		SwapMode:            SwapModeSequential,
		PayloadCode:         "SYN-PART-A",
		AllowedPayloadCodes: []string{"SYN-PART-A"},
		UOPCapacity:         120,
		ReorderPoint:        30,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("old Core decoded a new Edge's claim to %+v, want %+v — the unknown keys "+
			"must be dropped, and the known ones must survive beside them", got, want)
	}
}

// syntheticPressLineReport is a 12-claim process, shaped like a press line:
// three styles the cell can change over between, four claims each (two consume
// positions and two produce positions), all identifiers synthetic.
//
// It exists so the byte count below is taken on the shape the feed actually
// carries rather than on a single claim. Deterministic by construction —
// encoding/json writes struct fields in declaration order — so the encoded
// length is a number a test can pin.
func syntheticPressLineReport(withLegs func(i int) (string, string, string, string)) PlantClaimsReport {
	report := PlantClaimsReport{ProcessID: "SYN-PRESS-4", ConfigGen: 7}
	claim := 0
	for s := 0; s < 3; s++ {
		style := PlantClaimsStyle{
			StyleID: fmt.Sprintf("SYN-STYLE-%03d", s),
			Active:  s == 1,
		}
		for n := 0; n < 4; n++ {
			role, mode := ClaimRoleConsume, SwapModeSequential
			if n >= 2 {
				role, mode = ClaimRoleProduce, SwapModeTwoRobot
			}
			c := PlantClaim{
				CoreNodeName:        fmt.Sprintf("SYN-PLN_%03d", n),
				Role:                role,
				SwapMode:            mode,
				PayloadCode:         fmt.Sprintf("SYN-PART-%03d-%d", s, n),
				AllowedPayloadCodes: []string{fmt.Sprintf("SYN-PART-%03d-%d", s, n)},
				UOPCapacity:         120,
				ReorderPoint:        30,
			}
			if withLegs != nil {
				in, out, paired, second := withLegs(claim)
				applyPressLineLegs(&c, in, out, paired, second)
			}
			style.Claims = append(style.Claims, c)
			claim++
		}
		report.Styles = append(report.Styles, style)
	}
	return report
}

// pressLineLegs is the populated leg set the byte count uses — one synthetic
// supermarket each way and a two-position pairing, which is the heaviest shape
// a press cell configures.
func pressLineLegs(i int) (inbound, outbound, paired, second string) {
	return fmt.Sprintf("SYN-SMN_%03d", i),
		fmt.Sprintf("SYN-SMN_%03d", i+100),
		fmt.Sprintf("SYN-PLN_%03d", i+10),
		fmt.Sprintf("SYN-PLN_%03d", i+20)
}

// pressLineReportBytesUnset is the encoded size of syntheticPressLineReport
// with NO claim reporting a leg — the shape every plant sends today, and the
// shape every plant that never configures a leg keeps sending.
//
// THIS IS THE NUMBER THAT MATTERS FOR THE FLEET. The populated case is paid by
// the cells that configure legs; this one is paid by all of them, on every
// spec edit and every hourly snapshot, on a Pi. It is pinned as a literal so
// that adding a required field to PlantClaim, PlantClaimsStyle or
// PlantClaimsReport fails here instead of quietly raising the floor.
const pressLineReportBytesUnset = 2438

func TestPlantClaimsReport_UnsetPressLineIsUnchangedOnTheWire(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(syntheticPressLineReport(nil))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(b) != pressLineReportBytesUnset {
		t.Errorf("a 12-claim press line reporting no legs encodes to %d bytes, want %d.\n"+
			"Every claim in this feed is sent on every spec edit and every hourly snapshot, "+
			"from a Pi. If this grew, a field was added without omitempty and the whole fleet "+
			"is paying for it. If it shrank, say so in the commit — it is still a wire change.",
			len(b), pressLineReportBytesUnset)
	}
}

// applyPressLineLegs and pressLineLegsByteDelta are the ONE SEAM in this file
// that the leg fields move.
//
// It pinned a zero delta at 5cc7fd0c: PlantClaim had no leg fields, so there
// was nowhere to put the four values the byte-cost test generates and the
// populated report was byte-identical to the unset one. That zero was not an
// oversight in the test — it WAS the finding the S0 loop compiler was blocked
// on, Core mirroring nodes with no arcs between them. The fields exist now, so
// this fills them in and the constant below is the measured per-process cost of
// a fully-configured press line.
func applyPressLineLegs(c *PlantClaim, inbound, outbound, paired, second string) {
	c.InboundSource, c.OutboundDestination = inbound, outbound
	c.PairedCoreNode, c.SecondPairedCoreNode = paired, second
}

// pressLineLegsByteDelta is what a fully-configured 12-claim press line costs
// over the same line reporting no legs — the bill a cell that actually
// configures its loop pays, once per publish of that process.
const pressLineLegsByteDelta = 1692

// TestPlantClaimsReport_PopulatedPressLineByteCost reports what the legs cost
// when a cell actually configures them, so the number is on the record rather
// than estimated. It is an equality check for the same reason as the one
// above: the delta is a fact about the wire, and a fact that drifts silently
// is not being measured.
func TestPlantClaimsReport_PopulatedPressLineByteCost(t *testing.T) {
	t.Parallel()
	unset, err := json.Marshal(syntheticPressLineReport(nil))
	if err != nil {
		t.Fatalf("marshal unset: %v", err)
	}
	populated, err := json.Marshal(syntheticPressLineReport(pressLineLegs))
	if err != nil {
		t.Fatalf("marshal populated: %v", err)
	}
	delta := len(populated) - len(unset)
	if delta != pressLineLegsByteDelta {
		t.Errorf("legs on all 12 claims cost %d bytes (%d → %d), want %d — "+
			"the per-process cost of this feed is a measured number, not an estimate",
			delta, len(unset), len(populated), pressLineLegsByteDelta)
	}
}
