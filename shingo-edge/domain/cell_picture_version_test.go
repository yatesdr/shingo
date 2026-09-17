package domain

import (
	"testing"

	"shingo/protocol"
)

// cell_picture_version_test.go — the token the poll carries instead of the
// picture, and the doors it has to move on.
//
// WHAT THIS IS FOR. The station view used to carry the whole cell picture,
// rebuilt on every poll of every board — two queries and a deep copy of the
// plant's NGRP map at 500 ms a board on a Pi with one SQLite connection. It
// carries this string instead, and the page fetches the picture only when the
// string it holds stops matching. That is only safe if the string moves
// whenever the picture would, so each door below is a test rather than a
// paragraph.
//
// THE OTHER HALF OF THE SAFETY IS SOMEWHERE ELSE. The node generation is
// published by every writer of process_nodes and held honest by
// store/processes/node_generation_drift_test.go; here it is an input.

func versionInput() CellPictureVersionInput {
	return CellPictureVersionInput{
		StationID:       5,
		ProcessID:       1,
		ActiveStyleID:   7,
		NodeGeneration:  100,
		PlantGeneration: 200,
		Claims: []NodeClaim{
			{StyleID: 7, CoreNodeName: "PLN_01", SwapMode: protocol.SwapModeTwoRobotPressIndex,
				PayloadCode: "PART-A", PairedCoreNode: "PLN_02",
				InboundSource: "SMN_A", OutboundDestination: "SMN_B"},
			{StyleID: 7, CoreNodeName: "PLN_04", SwapMode: protocol.SwapModeTwoRobot,
				PayloadCode: "PART-B", InboundStaging: "PLN_05",
				InboundSource: "SMN_A", OutboundDestination: "SMN_B"},
			{StyleID: 9, CoreNodeName: "PLN_01", SwapMode: protocol.SwapModeTwoRobot,
				PayloadCode: "PART-C", InboundStaging: "PLN_02"},
		},
	}
}

// A VERSION IS NOT A TIMESTAMP. Two builds over the same facts have to agree,
// or every poll would send every board to fetch a picture nothing changed.
func TestCellPictureVersion_IsStableOverTheSameFacts(t *testing.T) {
	in := versionInput()
	if a, b := CellPictureVersion(in), CellPictureVersion(versionInput()); a != b {
		t.Fatalf("two builds over the same facts gave %q and %q; every poll would refetch", a, b)
	}
	if CellPictureVersion(in) == "" {
		t.Fatal("the version is empty; the page has nothing to compare")
	}
}

// THE CLAIMS ARRIVE IN MAP ORDER. liveClaimsByStyle groups them into a Go map
// and Go randomises map iteration, so a version that hashed them in sequence
// would be a different string on every poll of the same unchanged cell — and
// every board would refetch its picture twice a second, which is worse than
// what this replaced.
func TestCellPictureVersion_DoesNotDependOnClaimOrder(t *testing.T) {
	in := versionInput()
	shuffled := versionInput()
	shuffled.Claims = []NodeClaim{in.Claims[2], in.Claims[0], in.Claims[1]}
	if a, b := CellPictureVersion(in), CellPictureVersion(shuffled); a != b {
		t.Errorf("the same claims in another order gave %q and %q", a, b)
	}
}

// THE DOORS. Each one is a thing that changes the picture, and each has to
// change the string. They are named for the event an engineer would recognise,
// not for the field, because the field is what a reader checks and the event is
// what a reader remembers.
func TestCellPictureVersion_MovesOnEveryDoor(t *testing.T) {
	base := CellPictureVersion(versionInput())

	for _, tc := range []struct {
		door  string
		apply func(*CellPictureVersionInput)
	}{
		{"a cutover — the running style changes", func(in *CellPictureVersionInput) {
			in.ActiveStyleID = 9
		}},
		{"a flow save moves a choreography", func(in *CellPictureVersionInput) {
			in.Claims[0].SwapMode = protocol.SwapModeSingleRobot
		}},
		{"a flow save moves a part onto another position", func(in *CellPictureVersionInput) {
			in.Claims[0].PayloadCode = "PART-Z"
		}},
		{"a flow save re-pairs a position", func(in *CellPictureVersionInput) {
			in.Claims[0].PairedCoreNode = "PLN_06"
		}},
		{"a flow save changes an inbound staging slot", func(in *CellPictureVersionInput) {
			in.Claims[1].InboundStaging = "PLN_06"
		}},
		{"a flow save changes an outbound staging slot", func(in *CellPictureVersionInput) {
			in.Claims[1].OutboundStaging = "PLN_07"
		}},
		{"a flow save changes the dock", func(in *CellPictureVersionInput) {
			in.Claims[0].OutboundDestination = "SMN_C"
		}},
		{"a claim is deleted", func(in *CellPictureVersionInput) {
			in.Claims = in.Claims[:2]
		}},
		{"a claim is added", func(in *CellPictureVersionInput) {
			in.Claims = append(in.Claims, NodeClaim{
				StyleID: 7, CoreNodeName: "PLN_06", SwapMode: protocol.SwapModeTwoRobot, PayloadCode: "PART-D"})
		}},
		{"a station's node list is set — process_nodes moved", func(in *CellPictureVersionInput) {
			in.NodeGeneration++
		}},
		{"a changeover start minted a position", func(in *CellPictureVersionInput) {
			in.NodeGeneration += 3
		}},
		{"the geometry cache was replaced", func(in *CellPictureVersionInput) {
			in.PlantGeneration++
		}},
		{"another station of the same cell", func(in *CellPictureVersionInput) {
			in.StationID = 6
		}},
	} {
		in := versionInput()
		tc.apply(&in)
		if got := CellPictureVersion(in); got == base {
			t.Errorf("%s: the version did not move (%q). Every board holding this string "+
				"goes on drawing the cell as it was, and nothing says so", tc.door, got)
		}
	}
}

// A CLAIM THAT MOVED FROM ONE STYLE TO ANOTHER is a different picture the
// moment either style is the running one, and the claims are keyed by style on
// the poll. The style id is part of each claim's contribution, so two claims
// that differ only by style cannot cancel out.
func TestCellPictureVersion_TellsTwoStylesApart(t *testing.T) {
	a := versionInput()
	b := versionInput()
	b.Claims[2].StyleID = 11
	if CellPictureVersion(a) == CellPictureVersion(b) {
		t.Error("moving a claim to another style left the version alone")
	}
}

// BackPositionNames replaces a query. The rule is the one
// store.ListBackPositionNames had: every position any LIVE claim of the
// process names as a partner slot — paired, second paired, inbound or outbound
// staging — whichever style is running.
func TestBackPositionNames_IsEveryPartnerSlotOfEveryLiveClaim(t *testing.T) {
	got := BackPositionNames(versionInput().Claims)
	for _, want := range []string{"PLN_02", "PLN_05"} {
		if !got[want] {
			t.Errorf("%s is named as a partner slot and is not a back position: %v", want, got)
		}
	}
	// A FRONT POSITION IS NOT A BACK POSITION because it carries a claim. Only
	// the four partner FIELDS make one, which is what the caption means.
	for _, no := range []string{"PLN_01", "PLN_04", "SMN_A", "SMN_B"} {
		if got[no] {
			t.Errorf("%s is a claim's own position or its dock, not a partner slot: %v", no, got)
		}
	}
	if len(got) != 2 {
		t.Errorf("back positions = %v, want exactly PLN_02 and PLN_05", got)
	}
}

// THE GROUPS TRAVEL WITH THE PICTURE, AND ONLY THE ONES IT NAMES.
//
// The whole plant's NGRP map used to be deep-copied under the core-node lock
// and serialised into every poll of every board, and its one reader — the dock
// strip's member line — indexes at most two entries of it: the inbound source
// and the outbound destination the picture's claims carry.
func TestBuildCellPicture_CarriesOnlyTheGroupsItsClaimsName(t *testing.T) {
	claims := map[string]NodeClaim{
		"PLN_01": {CoreNodeName: "PLN_01", SwapMode: protocol.SwapModeTwoRobot,
			InboundSource: "SMN_SOURCE", OutboundDestination: "SMN_DEST"},
	}
	pic := BuildCellPicture(CellPictureInput{
		StationID: 5,
		Nodes: []Node{
			{CoreNodeName: "PLN_01", Sequence: 1, Enabled: true, OperatorStationID: ptrInt64(5)},
		},
		Claims: claims,
		Groups: map[string][]string{
			"SMN_SOURCE": {"SMN_01", "SMN_02"},
			"SMN_DEST":   {"SMN_09"},
			"SMN_ELSE":   {"SMN_77"},
			"PLN_FAR":    {"PLN_99"},
		},
	})
	if len(pic.Groups) != 2 {
		t.Fatalf("the picture carries %d groups (%v); it names two", len(pic.Groups), pic.Groups)
	}
	if got := pic.Groups["SMN_SOURCE"]; len(got) != 2 {
		t.Errorf("the source group lost its members: %v", got)
	}
	if _, carried := pic.Groups["SMN_ELSE"]; carried {
		t.Error("a group no claim of this picture names is on the picture")
	}
}

func ptrInt64(v int64) *int64 { return &v }
