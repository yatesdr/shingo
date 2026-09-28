package seerrds

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// privateIPv4Re is the Go twin of scripts/check-plant-addresses.sh, with the
// same generic rule: any RFC 1918 address (10/8, 172.16/12, 192.168/16). It
// names no network in particular, because a guard that listed the networks it
// protects would itself be the leak. Fixtures here use TEST-NET (RFC 5737).
// This test exists so CI fails on a live capture committed as a fixture even
// where the gate's scripts step does not run.
//
// Go's RE2 has no lookaround, so the boundaries are consumed in the match and
// the address is capture group 2.
var privateIPv4Re = regexp.MustCompile(
	`(^|[^0-9.])((?:10\.` + octet + `\.` + octet + `|172\.(?:1[6-9]|2[0-9]|3[01])\.` + octet +
		`|192\.168\.` + octet + `)\.` + octet + `)(?:[^0-9.]|\.[^0-9]|\.?$)`)

const octet = `(?:25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])`

// exemptDocRange is the one exemption the shell guard makes: 192.168.1.0/24,
// the example range the docs use. A fixture has no business in it either, but
// the rule is kept identical to the shell guard's so the two cannot disagree.
func exemptDocRange(addr string) bool { return strings.HasPrefix(addr, "192.168.1.") }

func privateHits(data string) []string {
	var out []string
	for _, m := range privateIPv4Re.FindAllStringSubmatch(data, -1) {
		if !exemptDocRange(m[2]) {
			out = append(out, m[2])
		}
	}
	return out
}

func TestScanFixturesForPrivateAddresses(t *testing.T) {
	t.Parallel()

	// Liveness first (same reason as the shell guard): a pattern that matches
	// nothing prints the same pass as a clean tree. Assembled so this file
	// stays clean under both scans.
	dq := func(a, b, c, d string) string { return a + "." + b + "." + c + "." + d }
	for _, bad := range []string{
		dq("10", "0", "0", "5"), dq("172", "16", "0", "1"), dq("172", "31", "9", "9"),
		dq("192", "168", "0", "1"), `"ip":"` + dq("10", "20", "30", "40") + `"`,
	} {
		if len(privateHits(bad)) == 0 {
			t.Fatalf("private-address pattern is dead: missed %q", bad)
		}
	}
	for _, ok := range []string{
		dq("192", "168", "1", "76"), dq("192", "0", "2", "1"), dq("198", "51", "100", "7"),
		dq("203", "0", "113", "9"), dq("172", "15", "0", "1"), dq("127", "0", "0", "1"),
		dq("110", "1", "2", "3"), dq("10", "1", "2", "3") + ".4",
	} {
		if h := privateHits(ok); len(h) != 0 {
			t.Fatalf("private-address pattern over-matches: flagged %q in %q", h, ok)
		}
	}

	files, err := filepath.Glob(filepath.Join("testdata", "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no testdata files found — scan is not looking at anything (fixtures live in this package's testdata/)")
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if h := privateHits(string(data)); len(h) != 0 {
			t.Errorf("%s carries private IPv4 addresses %q — derive fixtures with "+
				"scripts/anonymise-robotsstatus-fixture.py (TEST-NET only); this scan is the CI twin "+
				"of scripts/check-plant-addresses.sh", filepath.Base(f), h)
		}
	}
}
