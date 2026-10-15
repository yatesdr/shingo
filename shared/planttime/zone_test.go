package planttime

import "testing"

// TestResolveZone pins the precedence www and the engine both read: env, then
// the yaml, then UTC — and that a name which does not load is UTC and not Set.
func TestResolveZone(t *testing.T) {
	chicago(t) // skips without tzdata
	cases := []struct {
		name, env, cfg string
		wantZone       string
		wantSource     string
		wantSet        bool
		wantInvalid    string
	}{
		{"nothing set", "", "", "UTC", SourceDefault, false, ""},
		{"config only", "", "America/Chicago", "America/Chicago", SourceConfig, true, ""},
		{"env beats config", "America/New_York", "America/Chicago", "America/New_York", SourceEnv, true, ""},
		{"bad config", "", "Mars/Olympus", "UTC", SourceDefault, false, "Mars/Olympus"},
		{"bad env does not fall to config", "Mars/Olympus", "America/Chicago", "UTC", SourceDefault, false, "Mars/Olympus"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(EnvZone, c.env)
			z := ResolveZone(c.cfg)
			if z.Loc.String() != c.wantZone || z.Source != c.wantSource || z.Set() != c.wantSet || z.Invalid != c.wantInvalid {
				t.Errorf("ResolveZone(%q) env %q = {%s %s set=%v invalid=%q}, want {%s %s set=%v invalid=%q}",
					c.cfg, c.env, z.Loc, z.Source, z.Set(), z.Invalid, c.wantZone, c.wantSource, c.wantSet, c.wantInvalid)
			}
		})
	}
}
