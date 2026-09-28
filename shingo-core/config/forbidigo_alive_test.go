package config_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// forbidigo_alive_test.go — every forbidigo pattern must match at least one
// non-test Go declaration in the tree, except a named tombstone list.
//
// WHY THIS EXISTS. The 2026-09-26 dead-pattern sweep found FIVE forbidigo
// rules matching nothing, whose target primitives had been deleted:
// db.CancelOrderAtomic, binsstore.Unclaim (both deleted), nodes.ClaimSlot /
// db.ClaimSlot (deleted), bins.Move. The guards stayed behind as noise. A lint rule that
// matches nothing is worse than no rule — it reads as enforced coverage while
// guarding nothing, and it teaches everyone that the forbid list is
// decorative. The sweep also found the inverse hazard: live writers the
// rules SHOULD have fenced going unguarded (db.UpdateBin etc.), which is how
// the four raw-writer patterns below were born.
//
// Both hazards are mechanical to detect, so this test detects them:
//
//   - a pattern matching no non-test declaration is dead (fail, unless
//     tombstoned below with a reason);
//   - the tombstone list is checked for staleness too: a tombstone whose
//     pattern HAS a live declaration match means the primitive came back and
//     the guard is live again — move it out of the tombstone list (fail).
//
// Scope: non-test .go files across all five modules. Declaration matching is
// deliberately textual on the identifier the pattern's selector names —
// forbidigo matches selector expressions in CALL position at lint time, but
// a declaration existing somewhere is the cheap necessary condition for any
// call to ever exist; the census keeps this honest without a type checker.
//
// VERIFIED RED BY: adding a pattern matching nothing (e.g. `db\.NopeFoo`)
// in a scratch edit — the tombstone-aware census fails naming it.

// forbidigoTombstones are patterns that deliberately match nothing, each
// with the reason. These are REINTRODUCTION guards: the function they name
// was deleted, and the pattern exists to trip on its return. The rule's own
// `msg` in .golangci.yml must say so too (tombstoneMsgMarker), so a developer
// tripping it is told the function is gone rather than sent to use it.
var forbidigoTombstones = map[string]string{
	`bins\.Unclaim`: "forbids re-adding a deleted function: bins.Unclaim / bins.UnclaimByOrder (the bare claim clear that left the coupled reservation behind) were deleted 2026-09-26. If this test reports the pattern is live again, the function came back — move the row out of the tombstone list and clear its sanctioned callers.",
}

// tombstoneMsgMarker is the phrase a tombstone rule's msg must carry.
const tombstoneMsgMarker = "forbids re-adding a deleted function"

// TestForbidigoPatternsAreAlive parses .golangci.yml's forbidigo.forbid
// block and fails on any dead (matchless) pattern without a tombstone row.
func TestForbidigoPatternsAreAlive(t *testing.T) {
	rules := parseForbidRules(t)
	declText := readAllNonTestGo(t)

	dead := []string{}
	for _, r := range rules {
		p := r.pattern
		re, err := regexp.Compile(p)
		if err != nil {
			t.Errorf("forbidigo pattern %q does not compile: %v", p, err)
			continue
		}
		if re.MatchString(declText) {
			continue // alive
		}
		if _, ok := forbidigoTombstones[p]; ok {
			continue // deliberately dead, reason recorded
		}
		dead = append(dead, p)
	}
	for _, p := range dead {
		t.Errorf("forbidigo pattern %q matches no non-test declaration — it is dead. Delete it, or if it guards a deleted primitive's REINTRODUCTION, add it to forbidigoTombstones in this test with the reason.", p)
	}
}

// TestForbidigoTombstonesAreStillDead fails when a tombstoned pattern has a
// live declaration match: the primitive returned, the guard is live again,
// and the tombstone row (plus its "deleted" rationale) is now false.
func TestForbidigoTombstonesAreStillDead(t *testing.T) {
	declText := readAllNonTestGo(t)
	for p, reason := range forbidigoTombstones {
		re, err := regexp.Compile(p)
		if err != nil {
			t.Fatalf("tombstone pattern %q does not compile: %v", p, err)
		}
		if re.MatchString(declText) {
			t.Errorf("tombstoned pattern %q has a live declaration match — the primitive it guards came back. %s", p, reason)
		}
	}
}

// TestForbidigoTombstonesSayWhatTheyAre pins the tombstone contract both
// ways: every tombstone row names a rule that exists in .golangci.yml, that
// rule's msg says it forbids re-adding a deleted function, and its reason here
// says the same; and no live (non-tombstone) rule claims to be a tombstone.
func TestForbidigoTombstonesSayWhatTheyAre(t *testing.T) {
	msgs := map[string]string{}
	for _, r := range parseForbidRules(t) {
		msgs[r.pattern] = r.msg
	}
	for p, reason := range forbidigoTombstones {
		msg, ok := msgs[p]
		if !ok {
			t.Errorf("tombstone %q has no rule in .golangci.yml — the guard it records is gone; delete the row", p)
			continue
		}
		if !strings.Contains(msg, tombstoneMsgMarker) {
			t.Errorf("tombstone %q: its .golangci.yml msg must say it %s (so whoever trips it learns the function is gone, not how to call it); msg is %q", p, tombstoneMsgMarker, msg)
		}
		if !strings.Contains(reason, tombstoneMsgMarker) {
			t.Errorf("tombstone %q: its reason in forbidigoTombstones must say it %s", p, tombstoneMsgMarker)
		}
	}
	for p, msg := range msgs {
		if _, ok := forbidigoTombstones[p]; !ok && strings.Contains(msg, tombstoneMsgMarker) {
			t.Errorf("rule %q's msg calls it a tombstone but it is not in forbidigoTombstones — list it, or drop the wording", p)
		}
	}
}

type forbidRule struct{ pattern, msg string }

// parseForbidRules extracts the `pattern: '...'` values (and each one's
// `msg:`) under the forbidigo: forbid: block. The file's own comment says the
// input key is `pattern:` (mapstructure), which is what this reads.
// Indentation is the block delimiter: forbidigo: sits at 4 spaces, its forbid:
// list at 6/8, and any line back at ≤4 spaces that is not blank/# ends the
// block. Fails when it parses nothing, or a rule with no msg.
func parseForbidRules(t *testing.T) []forbidRule {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", ".golangci.yml"))
	if err != nil {
		t.Fatalf("read .golangci.yml: %v", err)
	}
	var out []forbidRule
	inForbid := false
	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if indent <= 4 && !strings.HasPrefix(trimmed, "-") {
			inForbid = trimmed == "forbidigo:"
			continue
		}
		if !inForbid {
			continue
		}
		if val, ok := strings.CutPrefix(trimmed, "- pattern:"); ok {
			out = append(out, forbidRule{pattern: strings.Trim(strings.TrimSpace(val), `'"`)})
			continue
		}
		if val, ok := strings.CutPrefix(trimmed, "msg:"); ok && len(out) > 0 {
			out[len(out)-1].msg = strings.Trim(strings.TrimSpace(val), `'"`)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no forbidigo patterns parsed from .golangci.yml — the parser rotted; fix it alongside the config")
	}
	for _, r := range out {
		if r.msg == "" {
			t.Fatalf("forbidigo pattern %q parsed with no msg — the parser rotted, or the rule lost its message", r.pattern)
		}
	}
	return out
}

// readAllNonTestGo concatenates every non-test .go file's selector-shaped
// text across the five modules. Selector matching only needs the text where
// declarations and call sites live; comments are stripped so a comment
// narrating `db.Foo` cannot fake life (the same rule the claimed_by census
// applies).
func readAllNonTestGo(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	var sb strings.Builder
	for _, mod := range []string{"protocol", "shared", "shingo-core", "shingo-edge", "integration"} {
		err := filepath.WalkDir(filepath.Join(root, mod), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			sb.WriteString(stripComments(string(b)))
			sb.WriteByte('\n')
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", mod, err)
		}
	}
	return sb.String()
}

// stripComments removes // and /* */ comments. Sufficient for a census: it
// needs only to never remove code.
func stripComments(src string) string {
	var out []byte
	for i := 0; i < len(src); {
		switch {
		case strings.HasPrefix(src[i:], "//"):
			j := strings.IndexByte(src[i:], '\n')
			if j < 0 {
				i = len(src)
			} else {
				i += j
			}
		case strings.HasPrefix(src[i:], "/*"):
			j := strings.Index(src[i+2:], "*/")
			if j < 0 {
				i = len(src)
			} else {
				i += j + 4
			}
		default:
			out = append(out, src[i])
			i++
		}
	}
	return string(out)
}
