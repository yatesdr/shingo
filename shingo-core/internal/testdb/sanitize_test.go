package testdb

import "testing"

// TestSanitizeIdent_MakesNamesUsableInCreateDatabase pins the fix for a bug
// that took out twelve CI shards at once.
//
// $SHINGO_TEST_PG_TEMPLATE is spliced into `CREATE DATABASE %s` unquoted. A
// workflow change built the value from a cache-key prefix containing hyphens,
// and Postgres answered `syntax error at or near "-"` on every shard — which
// reads like the whole suite breaking and is actually a database name.
func TestSanitizeIdent_MakesNamesUsableInCreateDatabase(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"already clean", "template_test", "template_test"},
		// The exact value CI produced.
		{"hyphens become underscores", "template_ci_123_shard-timings-core-race-4", "template_ci_123_shard_timings_core_race_4"},
		{"dots and slashes too", "tmpl.a/b", "tmpl_a_b"},
		{"uppercase folds down, as Postgres would", "Template_CI", "template_ci"},
		{"leading digit gets a prefix", "9lives", "t_9lives"},
		{"empty falls back", "", "template_test"},
		{"all junk falls back to something creatable", "---", "___"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sanitizeIdent(c.in, "template_test"); got != c.want {
				t.Errorf("sanitizeIdent(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestSanitizeIdent_RespectsPostgresIdentifierLimit(t *testing.T) {
	long := ""
	for range 100 {
		long += "a"
	}
	got := sanitizeIdent(long, "template_test")
	// Postgres silently truncates at 63 bytes. Truncating here instead keeps
	// the name we CREATE identical to the one we later look up in pg_database
	// — otherwise the existence check misses and every process rebuilds the
	// template it was supposed to share.
	if len(got) != 63 {
		t.Errorf("len = %d, want 63 (Postgres truncates there, so we must too)", len(got))
	}
}

// TestTemplateName_SanitizesTheEnvironment covers the path that actually
// broke: the value arriving from the environment rather than a literal.
func TestTemplateName_SanitizesTheEnvironment(t *testing.T) {
	t.Setenv(envSharedTemplate, "template_ci_9-race-3")
	if got, want := templateName(), "template_ci_9_race_3"; got != want {
		t.Errorf("templateName() = %q, want %q", got, want)
	}
}

func TestTemplateName_DefaultsWhenUnset(t *testing.T) {
	t.Setenv(envSharedTemplate, "")
	if got, want := templateName(), templateDBName; got != want {
		t.Errorf("templateName() = %q, want %q", got, want)
	}
}
