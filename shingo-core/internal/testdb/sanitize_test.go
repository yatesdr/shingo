package testdb

import (
	"math"
	"strings"
	"testing"
)

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

// TestTestDBName_TwoSubtestsSharingTheirFirstFortyCharactersGetTwoNames pins
// the fix for U12's gate failure (2026-11-14): two subtests of
// TestScenario_KeepStagedRecovery_AfterCancelledChangeover, in one process,
// asked Open for one database name ("already exists", SQLSTATE 42P04).
// sanitize cuts a test name to 40 characters and the pid is the process's, so
// all that told the subtests apart was a 1-in-100k random number. This
// test's own name is longer than 40, so its two subtests share the cut.
func TestTestDBName_TwoSubtestsSharingTheirFirstFortyCharactersGetTwoNames(t *testing.T) {
	var tests, names []string
	for _, sub := range []string{"consume/sequential/h", "produce/sequential/h"} {
		t.Run(sub, func(t *testing.T) {
			tests = append(tests, t.Name())
			names = append(names, nextTestDBName(t.Name()))
		})
	}
	if len(names) != 2 {
		t.Fatalf("ran %d subtests, want 2", len(names))
	}
	if a, b := sanitize(tests[0]), sanitize(tests[1]); a != b {
		t.Fatalf("the premise is gone: %q and %q cut to %q and %q, not one 40-character name", tests[0], tests[1], a, b)
	}
	if names[0] == names[1] {
		t.Errorf("subtests %q and %q both got database %q", tests[0], tests[1], names[0])
	}
	for _, n := range names {
		if len(n) > pgIdentMax || strings.Trim(n, "abcdefghijklmnopqrstuvwxyz0123456789_") != "" {
			t.Errorf("database name %q: %d bytes; want at most %d, of a-z, 0-9 and _ only", n, len(n), pgIdentMax)
		}
	}
}

// TestTestDBName_OneProcessNeverRepeatsAName asks for more names than the old
// random suffix had values (100,000), for one test name in one process, and
// finds no two alike. The old name builder fails this every run, by
// pigeonhole, not once in a hundred thousand.
func TestTestDBName_OneProcessNeverRepeatsAName(t *testing.T) {
	const n = 100_001
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		name := nextTestDBName(t.Name())
		if seen[name] {
			t.Fatalf("name %d repeats %q", i+1, name)
		}
		seen[name] = true
	}
}

// TestTestDBName_FitsPostgresWithTheSuffixWhole: at the longest pid and seq,
// the name is inside Postgres's 63 bytes and its unique suffix is whole; the
// test name is what gives way. Postgres would cut a longer name silently, and
// two names that differ only past byte 63 are one database.
func TestTestDBName_FitsPostgresWithTheSuffixWhole(t *testing.T) {
	long := strings.Repeat("TestAVeryLongName_", 5)
	got := testDBName(long, math.MaxUint32, math.MaxUint64)
	if len(got) > pgIdentMax {
		t.Errorf("%q is %d bytes; Postgres keeps %d", got, len(got), pgIdentMax)
	}
	if suffix := "_p4294967295_18446744073709551615"; !strings.HasSuffix(got, suffix) {
		t.Errorf("%q lost its suffix %q, the part that makes it unique", got, suffix)
	}
	if got, want := testDBName("TestShort", 4242, 7), "test_testshort_p4242_7"; got != want {
		t.Errorf("testDBName = %q, want %q", got, want)
	}
}
