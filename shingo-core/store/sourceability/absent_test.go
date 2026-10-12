package sourceability

import (
	"reflect"
	"testing"

	"shingocore/store/plantclaims"
)

// absent_test.go — D2.5: a red verdict says which of its missing payloads have
// no bin anywhere, free or spoken for (Absent), so the Edge's dry-source guard
// reads the feed instead of asking Core.

// A style missing two payloads, one with stock all spoken for and one with no
// bin at all, names only the second absent. A green style names nothing.
func TestCompute_AbsentIsTheMissingWithNoBinAtAll(t *testing.T) {
	red, green := key("SNF2", "A"), key("SNF2", "B")
	in := Inputs{
		Styles: []plantclaims.ProcessKey{red, green},
		Claims: map[plantclaims.ProcessKey][]plantclaims.ClaimRow{
			red:   {claim("N1", "BIN-BUSY", 0), claim("N2", "BIN-NONE", 1)},
			green: {claim("N3", "BIN-FREE", 0)},
		},
		Pool:    map[string]int{"BIN-FREE": 1},
		Present: map[string]int{"BIN-FREE": 1, "BIN-BUSY": 3},
	}
	got := byKey(Compute(in, Config{}, now))
	if !reflect.DeepEqual(got[red].Missing, []string{"BIN-BUSY", "BIN-NONE"}) {
		t.Fatalf("missing = %v, want [BIN-BUSY BIN-NONE]", got[red].Missing)
	}
	if !reflect.DeepEqual(got[red].Absent, []string{"BIN-NONE"}) {
		t.Errorf("absent = %v, want [BIN-NONE]: BIN-BUSY has three bins spoken for, which is a wait", got[red].Absent)
	}
	if got[green].Absent != nil {
		t.Errorf("a green style names absent payloads %v", got[green].Absent)
	}
}
