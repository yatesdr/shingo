//go:build docker

package schemadump_test

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"shingocore/internal/schemadump"
	"shingocore/store"
)

// TestBaselineIsCurrent regenerates the baseline and compares it with the
// committed file, byte for byte. The baseline is generated, never edited, and
// nobody will remember to regenerate it, so this test remembers and names the
// command that does.
//
// It renders twice, on two databases, and requires the two to agree first: a
// generator whose output moves between runs would make this test fail on a
// tree nobody touched (the seed rows' times are why the stamp exists). And it
// scans the committed file for what a public repository must not carry.
func TestBaselineIsCurrent(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	committed, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(schemadump.BaselinePath)))
	if err != nil {
		t.Fatalf("read %s: %v\n\nrun `%s` and commit the result", schemadump.BaselinePath, err, schemadump.BaselineRegenCommand)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	inst := start(ctx, t)

	first, err := inst.GenerateBaseline(ctx, root, releaseWork)
	if err != nil {
		t.Fatalf("generate the baseline: %v", err)
	}
	second, err := inst.GenerateBaseline(ctx, root, releaseWork)
	if err != nil {
		t.Fatalf("generate the baseline again: %v", err)
	}
	if first != second {
		t.Fatalf("two renders of the baseline differ, so the generator is not deterministic and the "+
			"staleness check below would fail on an untouched tree:\n%s", firstDiff(first, second, "first", "second"))
	}

	if leaks := publicRepoLeaks(string(committed)); len(leaks) > 0 {
		t.Errorf("the committed baseline carries what a public repository must not:\n  %s",
			strings.Join(leaks, "\n  "))
	}

	want := strings.ReplaceAll(string(committed), "\r\n", "\n")
	if first != want {
		t.Fatalf("the baseline is stale: run `%s` and commit the result\n\nfirst difference:\n%s",
			schemadump.BaselineRegenCommand, firstDiff(want, first, "committed", "generated"))
	}
}

var (
	ipv4Re       = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)
	bcryptRe     = regexp.MustCompile(`\$2[aby]\$\d\d\$`)
	privateKeyRe = regexp.MustCompile(`BEGIN [A-Z ]*PRIVATE KEY`)
	quotedRe     = regexp.MustCompile(`'((?:[^']|'')*)'`)
	tokenRe      = regexp.MustCompile(`[A-Za-z0-9+/=_-]{20,}`)
)

// publicRepoLeaks lists the lines of a baseline that look like a host, a
// credential or a key: an IPv4 address anywhere, a bcrypt hash, a private key,
// and, in the seed rows' string values, a long token of high entropy (a
// password or a secret does not look like a word). The seed rows are the only
// data the baseline carries, so they are where one would hide.
func publicRepoLeaks(baseline string) []string {
	var out []string
	for n, line := range strings.Split(baseline, "\n") {
		switch {
		case ipv4Re.MatchString(line):
			out = append(out, "line "+itoa(n+1)+": an IPv4 address: "+line)
		case bcryptRe.MatchString(line):
			out = append(out, "line "+itoa(n+1)+": a bcrypt hash")
		case privateKeyRe.MatchString(line):
			out = append(out, "line "+itoa(n+1)+": a private key")
		}
		if !strings.HasPrefix(line, "INSERT INTO ") {
			continue
		}
		for _, q := range quotedRe.FindAllStringSubmatch(line, -1) {
			for _, tok := range tokenRe.FindAllString(q[1], -1) {
				if shannon(tok) > 4.2 {
					out = append(out, "line "+itoa(n+1)+": a high-entropy token in a seed row: "+tok)
				}
			}
		}
	}
	return out
}

func shannon(s string) float64 {
	counts := map[rune]float64{}
	for _, r := range s {
		counts[r]++
	}
	var h float64
	n := float64(len(s))
	for _, c := range counts {
		p := c / n
		h -= p * math.Log2(p)
	}
	return h
}

// TestPublicRepoLeaks_FindsWhatItLooksFor is the scan's red half: a scan that
// has never found anything is a claim, not a check.
func TestPublicRepoLeaks_FindsWhatItLooksFor(t *testing.T) {
	t.Parallel()
	for _, line := range []string{
		"INSERT INTO public.edge_registry VALUES (1, 'x', '203.0.113.40');",
		"INSERT INTO public.admin_users VALUES (1, 'admin', '$2a$10$abcdefghijklmnopqrstuv');",
		"INSERT INTO public.secrets VALUES ('kX9vQ2mZ7pL4wR8tN3bJ6hF1');",
	} {
		if len(publicRepoLeaks(line)) == 0 {
			t.Errorf("the scan found nothing in %q", line)
		}
	}
	if leaks := publicRepoLeaks("INSERT INTO public.node_types VALUES (1, 'LANE', 'Lane (groups depth-ordered slots)', true, now(), now());"); len(leaks) > 0 {
		t.Errorf("the scan flags a plain seed row: %v", leaks)
	}
}

// TestSecondBootChangesNothing boots a new database, dumps its schema and its
// rows, boots it again and dumps again: the two must be identical. A boot is
// not a repair pass; on a database it has already brought up it writes
// nothing (the runner's verify reads, it does not write).
func TestSecondBootChangesNothing(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	inst := start(ctx, t)
	name, err := inst.BuildFresh(ctx)
	if err != nil {
		t.Fatalf("build a fresh database: %v", err)
	}
	before, err := inst.DumpWithRowsForTest(ctx, name)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	if !strings.Contains(before, "INSERT INTO public.schema_migrations VALUES ("+itoa(store.RetiredChain)+",") {
		t.Fatalf("the fresh database's dump records no v%d: the dump read no rows, so the comparison below would compare nothing", store.RetiredChain)
	}
	if err := inst.Reboot(ctx, name); err != nil {
		t.Fatalf("second boot: %v", err)
	}
	after, err := inst.DumpWithRowsForTest(ctx, name)
	if err != nil {
		t.Fatalf("dump after the second boot: %v", err)
	}
	if after != before {
		t.Fatalf("the second boot changed the database:\n%s", firstDiff(before, after, "after one boot", "after two"))
	}
}
