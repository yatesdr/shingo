package domain

import (
	"sort"
	"strings"
	"testing"

	"shingo/protocol"
)

// The claim no longer carries the directive setting — the station does, and the
// caller passes it in. `on` is kept in the signature so each test still reads as
// "a station that is / is not opted in".
func loaderClaim(on bool, payloads ...string) *NodeClaim {
	_ = on
	return &NodeClaim{
		CoreNodeName:        "SMN_014",
		Role:                protocol.ClaimRoleProduce,
		SwapMode:            protocol.SwapModeManualSwap,
		AllowedPayloadCodes: payloads,
	}
}

func pressToClaim(node, payload string) NodeClaim {
	return NodeClaim{CoreNodeName: node, Role: protocol.ClaimRoleProduce, PayloadCode: payload}
}

var binTypes = func(p string) string {
	switch p {
	case "PART-A":
		return "TOTE-L"
	case "PART-B":
		return "TOTE-S"
	case "PART-C":
		return "TOTE-L"
	}
	return ""
}

func TestBuildChangeoverLoadDirective_NamesTheBinTypeAndWho(t *testing.T) {
	t.Parallel()
	d := BuildChangeoverLoadDirective(77, true, loaderClaim(true, "PART-A", "PART-B"),
		[]NodeClaim{pressToClaim("PLN_002", "PART-A"), pressToClaim("PLN_004", "PART-B")}, binTypes)
	if d == nil {
		t.Fatal("want a directive")
	}
	if got := strings.Join(d.BinTypeCodes, ","); got != "TOTE-L,TOTE-S" {
		t.Errorf("bin types = %q, want TOTE-L,TOTE-S", got)
	}
	if got := strings.Join(d.ForNodes, ","); got != "PLN_002,PLN_004" {
		t.Errorf("waiting cells = %q, want PLN_002,PLN_004", got)
	}
	// The episode the resulting load belongs to — without it a changeover load
	// reads as an orphan replenishment in demand-origin reporting.
	if d.ChangeoverID != 77 {
		t.Errorf("ChangeoverID = %d, want 77", d.ChangeoverID)
	}
}

// Two cells on the same dunnage is ONE instruction. "Load TOTE-L + TOTE-L" is
// an instruction that reads as two trips.
func TestBuildChangeoverLoadDirective_DeduplicatesBinTypes(t *testing.T) {
	t.Parallel()
	d := BuildChangeoverLoadDirective(1, true, loaderClaim(true, "PART-A", "PART-C"),
		[]NodeClaim{pressToClaim("PLN_002", "PART-A"), pressToClaim("PLN_004", "PART-C")}, binTypes)
	if d == nil {
		t.Fatal("want a directive")
	}
	if got := strings.Join(d.BinTypeCodes, ","); got != "TOTE-L" {
		t.Errorf("bin types = %q, want a single TOTE-L", got)
	}
	// ...but both cells are still named, because both are waiting.
	if len(d.ForNodes) != 2 {
		t.Errorf("waiting cells = %v, want both", d.ForNodes)
	}
}

// SILENCE IS AN ANSWER. A card that always shows a directive is a card whose
// directive nobody reads.
func TestBuildChangeoverLoadDirective_SaysNothingWhenItHasNothingToSay(t *testing.T) {
	t.Parallel()
	tos := []NodeClaim{pressToClaim("PLN_002", "PART-A")}
	for _, tc := range []struct {
		name  string
		coID  int64
		on    bool
		claim *NodeClaim
		tos   []NodeClaim
		bt    func(string) string
	}{
		{"no active changeover", 0, true, loaderClaim(true, "PART-A"), tos, binTypes},
		{"the station is not opted in", 1, false, loaderClaim(false, "PART-A"), tos, binTypes},
		{"no claim at all", 1, true, nil, tos, binTypes},
		{"nothing is changing over", 1, true, loaderClaim(true, "PART-A"), nil, binTypes},
		// A payload this loader does not serve is another loader operator's
		// instruction, not this one's.
		{"the incoming payload is not ours", 1, true, loaderClaim(true, "PART-Z"), tos, binTypes},
		// UNKNOWN IS NOT A BIN TYPE. The catalog arrives with the node-list
		// sync, so an Edge that has not heard from Core answers "" for
		// everything — naming a carrier we cannot identify sends the operator
		// to fetch a guess.
		{"the bin type is unknown", 1, true, loaderClaim(true, "PART-A"), tos, func(string) string { return "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if d := BuildChangeoverLoadDirective(tc.coID, tc.on, tc.claim, tc.tos, tc.bt); d != nil {
				t.Errorf("want no directive; got %+v", d)
			}
		})
	}
}

// AN UNLOADER GETS ONE TOO, if its station is opted in.
//
// This used to be refused on the reasoning that an unloader's instruction would
// be about fulls and so a different thing. It is not: the instruction is "for
// the payloads this station serves, here is the carrier the incoming style
// needs", which is the same sentence whichever way material flows through the
// station. Whether a given station wants to be told is the question the setting
// answers, and it is answered where the station is set up.
func TestBuildChangeoverLoadDirective_UnloaderGetsOneWhenOptedIn(t *testing.T) {
	t.Parallel()
	c := loaderClaim(true, "PART-A")
	c.Role = protocol.ClaimRoleConsume
	d := BuildChangeoverLoadDirective(1, true, c, []NodeClaim{pressToClaim("PLN_002", "PART-A")}, binTypes)
	if d == nil {
		t.Fatal("an opted-in unloader gets the directive; the role is not the question")
	}
	if len(d.BinTypeCodes) != 1 || d.BinTypeCodes[0] != "TOTE-L" {
		t.Errorf("bin types = %v, want [TOTE-L]", d.BinTypeCodes)
	}
}

// And a station that was never opted in gets nothing, whatever its role.
func TestBuildChangeoverLoadDirective_NotOptedInGetsNone(t *testing.T) {
	t.Parallel()
	if d := BuildChangeoverLoadDirective(1, false, loaderClaim(false, "PART-A"),
		[]NodeClaim{pressToClaim("PLN_002", "PART-A")}, binTypes); d != nil {
		t.Errorf("a station that was not opted in gets no directive; got %+v", d)
	}
}

// Only the cells that consume empties are waiting on a loader. A consume cell
// in the incoming style wants fulls and is nothing to do with this board.
func TestBuildChangeoverLoadDirective_IgnoresConsumeCells(t *testing.T) {
	t.Parallel()
	consumeCell := pressToClaim("WELD_01", "PART-A")
	consumeCell.Role = protocol.ClaimRoleConsume
	if d := BuildChangeoverLoadDirective(1, true, loaderClaim(true, "PART-A"), []NodeClaim{consumeCell}, binTypes); d != nil {
		t.Errorf("a consume cell does not put a loader to work; got %+v", d)
	}
}

// The card re-renders on every SSE tick and MUST NOT reshuffle.
//
// ASSERTED AS SORTEDNESS, not as "two calls agree". The caller builds its
// claim list by ranging a map (station_service's targetClaims), so input order
// is already arbitrary and re-running with the same slice proves nothing — the
// output has to be ordered regardless of what came in.
func TestBuildChangeoverLoadDirective_OutputIsSorted(t *testing.T) {
	t.Parallel()
	// Deliberately reverse-ordered input, the shape a map range can hand over.
	tos := []NodeClaim{
		pressToClaim("PLN_009", "PART-B"),
		pressToClaim("PLN_002", "PART-A"),
		pressToClaim("PLN_004", "PART-C"),
	}
	d := BuildChangeoverLoadDirective(1, true, loaderClaim(true, "PART-A", "PART-B", "PART-C"), tos, binTypes)
	if d == nil {
		t.Fatal("want a directive")
	}
	for _, tc := range []struct {
		name string
		got  []string
	}{
		{"bin types", d.BinTypeCodes},
		{"payloads", d.PayloadCodes},
		{"waiting cells", d.ForNodes},
	} {
		if !sort.StringsAreSorted(tc.got) {
			t.Errorf("%s are not sorted: %v — the card would reshuffle between SSE ticks", tc.name, tc.got)
		}
	}
	if got := strings.Join(d.ForNodes, ","); got != "PLN_002,PLN_004,PLN_009" {
		t.Errorf("waiting cells = %q, want them in order regardless of input order", got)
	}
}
