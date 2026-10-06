package domain

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingoedge/domain/flowspec"
)

// TestRefusalsSayTheModeWordTheScreensSay: a refusal names the swap mode with
// SwapModeWord, the same word the chip and the strip card show, so an operator
// reading the finding sees the mode they picked. One table, one spelling.
func TestRefusalsSayTheModeWordTheScreensSay(t *testing.T) {
	t.Parallel()
	routing := map[string]bool{}
	for _, f := range flowspec.RoutingFields() {
		routing[string(f)] = true
	}
	for _, mode := range protocol.ConfigurableSwapModes() {
		word := SwapModeWord(mode)
		if word == "" {
			continue
		}
		in := NodeClaimInput{
			StyleID: 1, CoreNodeName: "LINE", Role: protocol.ClaimRoleConsume,
			SwapMode: mode, PayloadCode: "PART",
		}
		seen := 0
		for _, f := range ValidateNodeClaim(in, ClaimNodeContext{}) {
			if !routing[f.Field] {
				continue
			}
			seen++
			if !strings.HasPrefix(f.Message, word+" requires ") {
				t.Errorf("%s: routing refusal %q does not lead with %q", mode, f.Message, word)
			}
		}
		if seen == 0 {
			t.Errorf("%s: a claim with no routing drew no routing refusal", mode)
		}
		if !strings.Contains(KeepStagedModesMessage, word) {
			t.Errorf("keep-staged refusal %q does not name %q", KeepStagedModesMessage, word)
		}
	}
}

// TestAMissingPartIsAskedForAsAPart: the screens say part, never payload, so
// the refusal for a consume claim with no part asks for a part.
func TestAMissingPartIsAskedForAsAPart(t *testing.T) {
	t.Parallel()
	in := NodeClaimInput{
		StyleID: 1, CoreNodeName: "LINE", Role: protocol.ClaimRoleConsume,
		SwapMode: protocol.SwapModeSingleRobot,
	}
	for _, f := range ValidateNodeClaim(in, ClaimNodeContext{}) {
		if f.Field == "payload_code" {
			if f.Message != "Select a part" {
				t.Fatalf("payload_code refusal = %q, want %q", f.Message, "Select a part")
			}
			return
		}
	}
	t.Fatal("a consume claim with no part was not refused on payload_code")
}
