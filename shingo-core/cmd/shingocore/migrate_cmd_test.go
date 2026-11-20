package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestMigrate_ACommandLineItDoesNotReadIsExit2: the installer tells a usage
// mistake from a refusal (exit 1) by the code, so a command line this does not
// read exits 2 with the usage on stderr, before any config or database.
func TestMigrate_ACommandLineItDoesNotReadIsExit2(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		nil,
		{"version"},
		{"check", "extra"},
		{"down"},
		{"down", "abc"},
		{"down", "0"},
		{"-nosuch", "check"},
		{"-help"},
	} {
		var stdout, stderr bytes.Buffer
		if code := runMigrate(args, &stdout, &stderr); code != 2 {
			t.Errorf("migrate %q: exit %d, want 2", args, code)
		}
		if !strings.Contains(stderr.String(), "Usage: shingocore migrate") || stdout.Len() != 0 {
			t.Errorf("migrate %q: stdout %q stderr %q, want the usage on stderr alone", args, stdout.String(), stderr.String())
		}
	}
}
