package www

import (
	"strings"
	"testing"
)

// TestPinPlantTimezone_OfferedToEdges pins the zone Core offers its Edges on
// the heartbeat ack (C9 b): PLANT_TIMEZONE when set, else the yaml, and empty
// when neither — never the UTC default. Before, Edges got the yaml alone while
// PLANT_TIMEZONE won on Core's own screens.
func TestPinPlantTimezone_OfferedToEdges(t *testing.T) {
	cases := []struct {
		name, env, yaml, want string
	}{
		{"env_set_wins_over_yaml", "America/New_York", "America/Chicago", "America/New_York"},
		{"env_set_yaml_blank", "America/New_York", "", "America/New_York"},
		{"env_unset_yaml", "", "America/Chicago", "America/Chicago"},
		{"env_unset_yaml_blank_offers_nothing", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PLANT_TIMEZONE", tc.env)
			if got := OfferedPlantTimezone(tc.yaml); got != tc.want {
				t.Errorf("OfferedPlantTimezone(%q) with PLANT_TIMEZONE=%q = %q, want %q", tc.yaml, tc.env, got, tc.want)
			}
		})
	}
}

// TestPinPlantTimezone_ConfigRowSaysOneClock: the timezone row's sub-label
// keeps fact (a) (Edges get the value Core read at start, after a restart) and
// no longer says Edges get something other than Core's screens (fact b).
func TestPinPlantTimezone_ConfigRowSaysOneClock(t *testing.T) {
	src := readSourceFile(t, "templates/config.html")
	i := strings.Index(src, `<label for="cfg-timezone">`)
	if i < 0 {
		t.Fatal("cfg-timezone label not found in config.html")
	}
	j := strings.Index(src[i:], "</label>")
	label := src[i : i+j]
	for _, gone := range []string{"not what Edges", "but not for Edges", "they get the saved value"} {
		if strings.Contains(label, gone) {
			t.Errorf("timezone sub-label still says %q", gone)
		}
	}
	if n := strings.Count(label, "so a change reaches them after Core restarts"); n != 2 {
		t.Errorf("fact (a) appears %d times in the sub-label, want 2 (env set and unset)", n)
	}
}
