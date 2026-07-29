package protocol_test

import (
	"testing"

	"shingo/protocol"
)

// THE THREE FORMATS ARE A WIRE CONTRACT — PINNED, NOT ROUND-TRIPPED.
//
// Once Core stores keys it did not compute, the episode-key FORMAT is a
// cross-service contract: Edge authors cell and changeover keys, Core stores
// and compares them, and Core's partial unique index enforces "one open
// episode per place" over the literal string. Change the format and old rows
// and new rows describe the same place differently — the index stops seeing
// them as one place, and a plant ends up with keys in both shapes. Version
// skew on a FORMAT rather than a field, which is the kind nobody looks for
// because nothing in protocol/ declares it as a field.
//
// A round-trip test cannot catch this. Every other test in this file builds a
// key with the constructors and parses it with ParseEpisodeKey, so it stays
// green under any format change as long as the two agree — self-consistency,
// not a contract. THIS test is the only thing standing between a format
// change and a silent split, so it hard-codes the strings: changing one has
// to be acknowledged in a diff rather than merged.
func TestEpisodeKeyFormats_ArePinned(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  string
		want string
	}{
		{
			name: "threshold reproduces Core's bindingKey shape",
			got:  protocol.ThresholdEpisodeKey("PLANT.LINE1", "SLN_002", "74577-6SA0A.06"),
			want: "thr|PLANT.LINE1|SLN_002|74577-6SA0A.06",
		},
		{
			name: "cell is keyed on the PROCESS, with direction in the identity",
			got:  protocol.CellEpisodeKey("PLANT.LINE1", 42, "PANEL-B", protocol.EpisodeDirectionSupply),
			want: "cell|PLANT.LINE1|42|PANEL-B|supply",
		},
		{
			name: "changeover is keyed on the changeover row",
			got:  protocol.ChangeoverEpisodeKey("PLANT.LINE1", 7),
			want: "co|PLANT.LINE1|7",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("episode key format CHANGED\n got: %s\nwant: %s\n\n"+
					"This is a cross-service wire contract, not an internal string. Edge authors "+
					"these keys and Core stores them verbatim under a unique index. If this change "+
					"is intended, it needs a migration for existing demand_origins rows and "+
					"coordinated deploys — not just a new expectation here.", tc.got, tc.want)
			}
		})
	}
}

// THREE KINDS, THREE IDENTITIES. This is why episode identity is a computed
// string and not a column tuple: a tuple wide enough for all three is mostly
// NULL, and NULLs do not participate in a unique index the way "one open
// episode per place" needs.
//
// The tuple version of this silently passed for two of the three kinds, which
// is why all three are tested here rather than one representative.
func TestEpisodeKeys_EveryKindIsParseable(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
		want protocol.ParsedEpisodeKey
	}{
		{
			name: "threshold",
			key:  protocol.ThresholdEpisodeKey("line-1", "SMN_001", "74577-6SA0A.06"),
			want: protocol.ParsedEpisodeKey{
				Kind: protocol.EpisodeKindThreshold, Station: "line-1",
				CoreNode: "SMN_001", Payload: "74577-6SA0A.06",
			},
		},
		{
			name: "cell supply",
			key:  protocol.CellEpisodeKey("line-1", 42, "PANEL-B", protocol.EpisodeDirectionSupply),
			want: protocol.ParsedEpisodeKey{
				Kind: protocol.EpisodeKindCell, Station: "line-1", ProcessID: 42,
				Payload: "PANEL-B", Direction: protocol.EpisodeDirectionSupply,
			},
		},
		{
			name: "cell evacuate",
			key:  protocol.CellEpisodeKey("line-1", 42, "ASSY", protocol.EpisodeDirectionEvacuate),
			want: protocol.ParsedEpisodeKey{
				Kind: protocol.EpisodeKindCell, Station: "line-1", ProcessID: 42,
				Payload: "ASSY", Direction: protocol.EpisodeDirectionEvacuate,
			},
		},
		{
			name: "changeover",
			key:  protocol.ChangeoverEpisodeKey("line-1", 907),
			want: protocol.ParsedEpisodeKey{
				Kind: protocol.EpisodeKindChangeover, Station: "line-1", ChangeoverID: 907,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := protocol.ParseEpisodeKey(tc.key)
			if err != nil {
				t.Fatalf("ParseEpisodeKey(%q): %v", tc.key, err)
			}
			if got != tc.want {
				t.Errorf("ParseEpisodeKey(%q)\n got %+v\nwant %+v", tc.key, got, tc.want)
			}
		})
	}
}

// A cell episode is per PROCESS and per DIRECTION. Both halves matter:
//
//   - Same process, same payload, different direction → two keys. A cell can
//     genuinely need material brought in and taken out at once, and those are
//     two demands.
//   - Different NODES in one process, same payload, same direction → ONE key.
//     That is the grain rule, and it is what makes an A/B pair's two claims
//     join one episode instead of minting two for the same need (O8).
func TestCellEpisodeKey_ProcessGrainAndDirection(t *testing.T) {
	supply := protocol.CellEpisodeKey("line-1", 42, "PANEL-B", protocol.EpisodeDirectionSupply)
	evac := protocol.CellEpisodeKey("line-1", 42, "PANEL-B", protocol.EpisodeDirectionEvacuate)
	if supply == evac {
		t.Fatal("direction must be part of the identity — in and out are two demands")
	}

	// The A/B case: PLN_003 and PLN_004 are two claims on one process for one
	// payload. Nothing about the node enters the key, so both resolve to the
	// same episode.
	a := protocol.CellEpisodeKey("line-1", 42, "PANEL-B", protocol.EpisodeDirectionSupply)
	b := protocol.CellEpisodeKey("line-1", 42, "PANEL-B", protocol.EpisodeDirectionSupply)
	if a != b {
		t.Fatal("two claims on one process must resolve to ONE episode key")
	}

	// Different processes are different places.
	if protocol.CellEpisodeKey("line-1", 43, "PANEL-B", protocol.EpisodeDirectionSupply) == supply {
		t.Error("two processes must not share an episode")
	}
	// And so are two stations.
	if protocol.CellEpisodeKey("line-2", 42, "PANEL-B", protocol.EpisodeDirectionSupply) == supply {
		t.Error("two stations must not share an episode")
	}
}

// The kinds must not collide with each other. A shared key would put two
// unrelated demands under one partial unique index entry, so the second would
// be rejected as a duplicate of something it has nothing to do with.
func TestEpisodeKeys_KindsDoNotCollide(t *testing.T) {
	keys := map[string]string{
		"threshold":  protocol.ThresholdEpisodeKey("line-1", "N1", "P"),
		"cell":       protocol.CellEpisodeKey("line-1", 1, "P", protocol.EpisodeDirectionSupply),
		"changeover": protocol.ChangeoverEpisodeKey("line-1", 1),
	}
	seen := map[string]string{}
	for kind, key := range keys {
		if other, dup := seen[key]; dup {
			t.Errorf("%s and %s produce the same key %q", kind, other, key)
		}
		seen[key] = kind
	}
}

// A key built by hand instead of through the constructors must be rejected
// rather than silently half-understood — that is the whole value of parsing it
// back. A mismatched key does not error at the database; it just fails to find
// the open episode and mints a second one.
func TestParseEpisodeKey_RejectsMalformed(t *testing.T) {
	for _, bad := range []string{
		"",
		"cell|line-1|42|PANEL-B",                // missing direction
		"cell|line-1|notanumber|PANEL-B|supply", // process id is not a number
		"cell|line-1|42|PANEL-B|sideways",       // direction is not a direction
		"thr|line-1|SMN_001",                    // missing payload
		"co|line-1",                             // missing id
		"threshold|line-1|SMN_001|P",            // the kind's NAME, not its prefix
		"line-1|SMN_001|P",                      // a bare bindingKey with no kind
	} {
		if got, err := protocol.ParseEpisodeKey(bad); err == nil {
			t.Errorf("ParseEpisodeKey(%q) accepted a malformed key as %+v", bad, got)
		}
	}
}
