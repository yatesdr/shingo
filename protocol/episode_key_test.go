package protocol_test

import (
	"strings"
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
			name: "threshold is the plant-unique Core node and the payload — NO station",
			got:  protocol.ThresholdEpisodeKey("SLN_002", "74577-6SA0A.06"),
			want: "thr|SLN_002|74577-6SA0A.06",
		},
		{
			// CHANGED DELIBERATELY, and this test is what required it to be
			// acknowledged: the fourth component was "supply"/"evacuate", a second
			// vocabulary for the claim's own role, and it is now the role itself.
			// The migration this failure demanded is v81; the coordinated-deploy
			// half is that nothing re-parses a stored cell key (Core parses inbound
			// wire only, Edge parses stored keys only for the changeover kind).
			name: "cell is keyed on the PROCESS, with the claim's ROLE in the identity — NO station",
			got:  protocol.CellEpisodeKey("SNF2", "PANEL-B", protocol.ClaimRoleConsume),
			want: "cell|SNF2|PANEL-B|consume",
		},
		{
			name: "and the produce half of the same process is a different key",
			got:  protocol.CellEpisodeKey("SNF2", "PANEL-B", protocol.ClaimRoleProduce),
			want: "cell|SNF2|PANEL-B|produce",
		},
		{
			name: "changeover is keyed on the changeover row, SCOPED by the station that counted it",
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
			key:  protocol.ThresholdEpisodeKey("SMN_001", "74577-6SA0A.06"),
			want: protocol.ParsedEpisodeKey{
				Kind:     protocol.EpisodeKindThreshold,
				CoreNode: "SMN_001", Payload: "74577-6SA0A.06",
			},
		},
		{
			name: "cell supply",
			key:  protocol.CellEpisodeKey("SNF2", "PANEL-B", protocol.ClaimRoleConsume),
			want: protocol.ParsedEpisodeKey{
				Kind: protocol.EpisodeKindCell, ProcessID: "SNF2",
				Payload: "PANEL-B", Role: protocol.ClaimRoleConsume,
			},
		},
		{
			name: "cell evacuate",
			key:  protocol.CellEpisodeKey("SNF2", "ASSY", protocol.ClaimRoleProduce),
			want: protocol.ParsedEpisodeKey{
				Kind: protocol.EpisodeKindCell, ProcessID: "SNF2",
				Payload: "ASSY", Role: protocol.ClaimRoleProduce,
			},
		},
		{
			name: "changeover",
			key:  protocol.ChangeoverEpisodeKey("line-1", 907),
			want: protocol.ParsedEpisodeKey{
				Kind: protocol.EpisodeKindChangeover, Station: "line-1", ChangeoverID: 907,
			},
		},
		{
			// The carrier type lands in BinType, NOT Payload. A maintained
			// group's demand is for an EMPTY carrier, and a reader finding
			// "45x58x32" in Payload would reasonably conclude the episode wanted
			// parts of that code in it.
			name: "maintain",
			key:  protocol.MaintainEpisodeKey("SYN_PRESS_EMPTIES", "45x58x32"),
			want: protocol.ParsedEpisodeKey{
				Kind:     protocol.EpisodeKindMaintain,
				CoreNode: "SYN_PRESS_EMPTIES", BinType: "45x58x32",
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
	supply := protocol.CellEpisodeKey("SNF2", "PANEL-B", protocol.ClaimRoleConsume)
	evac := protocol.CellEpisodeKey("SNF2", "PANEL-B", protocol.ClaimRoleProduce)
	if supply == evac {
		t.Fatal("direction must be part of the identity — in and out are two demands")
	}

	// The A/B case: PLN_003 and PLN_004 are two claims on one process for one
	// payload. Nothing about the node enters the key, so both resolve to the
	// same episode.
	a := protocol.CellEpisodeKey("SNF2", "PANEL-B", protocol.ClaimRoleConsume)
	b := protocol.CellEpisodeKey("SNF2", "PANEL-B", protocol.ClaimRoleConsume)
	if a != b {
		t.Fatal("two claims on one process must resolve to ONE episode key")
	}

	// Different processes are different places.
	if protocol.CellEpisodeKey("SNF3", "PANEL-B", protocol.ClaimRoleConsume) == supply {
		t.Error("two processes must not share an episode")
	}
	// TWO STATIONS ARE NOT TWO PLACES — the assertion that used to sit here
	// said they were, and it was wrong for exactly the reason the grain rule
	// gives. A process is one place needing one payload no matter which edge
	// happens to be reporting it, and a process that moves between edges must
	// keep its open episode. See TestEpisodeKeys_StationScopeByKind.
}

// THE STATION IS SCOPE FOR ONE KIND AND FRAGMENTATION FOR THE OTHER TWO, AND
// THIS TEST PINS THE ASYMMETRY IN BOTH DIRECTIONS.
//
// The rule is not "drop the station" — it is "the qualifier belongs in the key
// exactly when the thing being identified is not already plant-unique."
//
//   - THRESHOLD is keyed on a Core node name, and nodes.name is TEXT NOT NULL
//     UNIQUE plant-wide. The station adds nothing an identity needs and adds
//     one thing it must not have: a second edge id splitting one node's
//     episodes in two.
//   - CELL is keyed on the Edge process NAME, which Core ALREADY treats as
//     plant-unique — process_styles' PRIMARY KEY is (process_id, style_id) and
//     style_claims carries no station column at all. Keeping the station here
//     makes episode_key disagree with the very mirror that v63's BIGINT→TEXT
//     change was made to agree with.
//   - CHANGEOVER is keyed on process_changeovers.id, an EDGE-LOCAL SQLite row
//     id. Two edges both reach id 7. The station is the counter space that id
//     lives in, exactly as it is in inventory_delta_dedup, so it STAYS.
//
// Delete the station from all three and the changeover kind silently joins one
// plant's episode to another's under the partial unique index. That is the
// blanket-migration mistake, committed inside one 180-line file.
func TestEpisodeKeys_StationScopeByKind(t *testing.T) {
	t.Run("threshold ignores the station — the node name is plant-unique", func(t *testing.T) {
		a := protocol.ThresholdEpisodeKey("SLN_002", "74577-6SA0A.06")
		b := protocol.ThresholdEpisodeKey("SLN_002", "74577-6SA0A.06")
		if a != b {
			t.Fatalf("one node+payload must be one episode: %q vs %q", a, b)
		}
		if strings.Contains(a, "|line-") || strings.Contains(a, "plant-a") {
			t.Errorf("threshold key %q still carries a station component", a)
		}
	})

	t.Run("cell ignores the station — the process name is the grain", func(t *testing.T) {
		a := protocol.CellEpisodeKey("SNF2", "PANEL-B", protocol.ClaimRoleConsume)
		b := protocol.CellEpisodeKey("SNF2", "PANEL-B", protocol.ClaimRoleConsume)
		if a != b {
			t.Fatalf("one process+payload+direction must be one episode: %q vs %q", a, b)
		}
		// The grain rule the file's own doc comment states. A process that moves
		// from one edge to another is the SAME place needing the SAME material,
		// so the close must find the open.
		if strings.Count(a, "|") != 3 {
			t.Errorf("cell key %q has %d separators, want 3 (cell|process|payload|direction)",
				a, strings.Count(a, "|"))
		}
	})

	t.Run("changeover KEEPS the station — the row id is edge-local", func(t *testing.T) {
		one := protocol.ChangeoverEpisodeKey("EDGE-1", 7)
		two := protocol.ChangeoverEpisodeKey("EDGE-2", 7)
		if one == two {
			t.Fatalf("changeover id 7 on two edges must NOT be one episode — "+
				"process_changeovers.id is an Edge-local SQLite row id and both edges reach 7. "+
				"Both keys are %q, so one plant's changeover episode would join the other's "+
				"under the partial unique index on demand_origins(episode_key).", one)
		}
	})
}

// The kinds must not collide with each other. A shared key would put two
// unrelated demands under one partial unique index entry, so the second would
// be rejected as a duplicate of something it has nothing to do with.
func TestEpisodeKeys_KindsDoNotCollide(t *testing.T) {
	// Threshold and changeover are both three-part keys now, so the prefix is
	// the only thing keeping them apart — worth stating, since the collision
	// this test guards became one component closer after the station left two
	// of the three formats.
	keys := map[string]string{
		"threshold":  protocol.ThresholdEpisodeKey("N1", "P"),
		"cell":       protocol.CellEpisodeKey("P1", "P", protocol.ClaimRoleConsume),
		"changeover": protocol.ChangeoverEpisodeKey("line-1", 1),
		// maintain is a three-part key like threshold and changeover, and it is
		// keyed on a NODE NAME exactly as threshold is — so "thr" vs "mnt" is
		// the only thing separating a group's level episode from a threshold
		// episode on a node of the same name. That is a real shape: a loader
		// anchored at a node named like a group is not forbidden anywhere.
		"maintain": protocol.MaintainEpisodeKey("N1", "P"),
	}
	seen := map[string]string{}
	for kind, key := range keys {
		if other, dup := seen[key]; dup {
			t.Errorf("%s and %s produce the same key %q", kind, other, key)
		}
		seen[key] = kind
	}
}

// "cell|line-1|notanumber|PANEL-B|supply" IS NO LONGER MALFORMED and was
// removed from this list deliberately: the process component is the Edge process
// NAME now, so "notanumber" is a perfectly good one. What the removed
// fmt.Sscanf(parts[2], "%d") ALSO rejected was the empty string, and that is
// still a defect — a cell key with no process names no place and collides with
// every other such key under the partial unique index. The empty case took its
// place in this list rather than the coverage quietly shrinking by one.
//
// A key built by hand instead of through the constructors must be rejected
// rather than silently half-understood — that is the whole value of parsing it
// back. A mismatched key does not error at the database; it just fails to find
// the open episode and mints a second one.
func TestParseEpisodeKey_RejectsMalformed(t *testing.T) {
	for _, bad := range []string{
		"",
		"cell|SNF2|PANEL-B",          // missing direction
		"cell||PANEL-B|supply",       // no process — names no place
		"cell|SNF2|PANEL-B|sideways", // direction is not a direction
		"thr|SMN_001",                // missing payload
		"mnt|SYN_EMPTIES",            // missing carrier type
		"mnt||45x58x32",              // no group — names no place
		"mnt|SYN_EMPTIES|",           // no type — cannot say what it is short OF
		"mnt|SYN_EMPTIES|45x58|32",   // a type code carrying the separator
		"thr||74577-6SA0A.06",        // no Core node — names no place
		"co|line-1",                  // missing id
		"threshold|SMN_001|P",        // the kind's NAME, not its prefix
		"SMN_001|P",                  // a bare bindingKey with no kind

		// THE OLD FIVE- AND FOUR-PART SHAPES. A key written by the pre-change
		// code, or by a stale service during a skewed deploy, must be REJECTED
		// rather than parsed into the wrong components — "plant-a.line-1" would
		// otherwise land in ProcessID and name a process that does not exist.
		// demand_origins is unshipped so no stored key has these shapes, but a
		// mixed-version pair of services can still emit one.
		"cell|plant-a.line-1|SNF2|PANEL-B|supply",
		"thr|plant-a.line-1|SLN_002|74577-6SA0A.06",
	} {
		if got, err := protocol.ParseEpisodeKey(bad); err == nil {
			t.Errorf("ParseEpisodeKey(%q) accepted a malformed key as %+v", bad, got)
		}
	}
}
