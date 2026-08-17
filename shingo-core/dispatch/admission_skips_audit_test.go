package dispatch

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// admission_skips_audit_test.go — C4's standing guard on the skip set.
//
// The audit rule in admission.go: a caller may skip a physical question ONLY IF
// that question is answered elsewhere on its path, AT A NAMED LINE, and only if
// the refusal it would otherwise make has a named releaser. A skip with no line
// dies.
//
// That rule is enforced by review, which is exactly the kind of enforcement that
// stops happening. These two tests make the cheap half structural: the SHAPE of
// the set, and the presence of a justification beside every value that sets a
// skip. Whether a cited line really answers the question is still a human call —
// no test can make that one — but a skip added without any citation at all now
// fails the build.

// TestAdmissionSkips_OnlyConditionalSkipIsOccupancyWhenGated pins C4's
// conclusion.
//
// Two fields, and they are different KINDS of thing. reachability is a claim
// about what the CALLER already knows (the finder answered it). entryWhenGated
// is a claim about the LANE — the only skip whose effect depends on state read at
// decision time, which is why it is the only one that may be conditional.
//
// A third field is the thing to think twice about: the unconditional occupancy
// skip had a field and no setter for a whole branch, which meant the one skip the
// unification exists to make unavailable was still on offer to the next caller,
// with no audit line for them to write. Adding a field here means adding a way to
// not ask a physical question; the test says so out loud.
func TestAdmissionSkips_OnlyConditionalSkipIsOccupancyWhenGated(t *testing.T) {
	typ := reflect.TypeOf(admissionSkips{})
	got := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		got = append(got, typ.Field(i).Name)
	}
	want := []string{"reachability", "entryWhenGated"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("admissionSkips fields = %v, want %v.\n"+
			"A new field is a new way for a lane-entry path to not ask a physical question. It needs "+
			"an answering line and a named releaser on every caller that sets it (the audit rule in "+
			"admission.go), and this test updated deliberately — not to make it pass.", got, want)
	}

	// The zero value asks everything. This is the forgetting-is-safe property the
	// whole file is built on, and it is one line to check.
	var zero admissionSkips
	if zero.reachability || zero.entryWhenGated {
		t.Error("the zero admissionSkips skips something — a caller that forgets the field would " +
			"silently fail open, which is the shape this type exists to make unreachable")
	}
}

// TestAdmissionSkips_EverySetHasItsJustification checks that each declared skip
// set carries prose naming what answers the questions it does not ask.
//
// It reads the source rather than the values, because the thing under audit IS
// the prose: the values are three words and the justification is the work. A set
// declared with no comment block above it is the failure mode — a skip nobody
// had to explain.
func TestAdmissionSkips_EverySetHasItsJustification(t *testing.T) {
	src := readRepoFile(t, filepath.Join("shingo-core", "dispatch", "admission.go"))

	// Every declared skip set, and the word its justification must contain. The
	// word is the question being skipped: a set that skips reachability has to
	// say the word somewhere in its own doc block.
	sets := map[string]string{
		"skipsForPlainEntry":      "reachability",
		"skipsForComplexEntry":    "reachability",
		"skipsForGatedStoreEntry": "occupancy",
	}
	for name, mustMention := range sets {
		decl := "var " + name + " = admissionSkips{"
		at := strings.Index(src, decl)
		if at < 0 {
			t.Errorf("skip set %s is gone. If it was deleted deliberately, delete its entry here too; "+
				"if it was renamed, the audit rule follows the name", name)
			continue
		}
		doc := docBlockBefore(src[:at])
		if doc == "" {
			t.Errorf("%s has no doc block. Every skip set states what answers the questions it does "+
				"not ask, at a named line — that is the audit rule, and a set with no prose has "+
				"nothing to audit", name)
			continue
		}
		if !strings.Contains(strings.ToLower(doc), mustMention) {
			t.Errorf("%s's justification never mentions %q, which is the question it turns off",
				name, mustMention)
		}
		// A justification that cites nothing is a claim, not an answer. Every real
		// one in this file points at a file, a function, or both.
		if !strings.Contains(doc, ".go") && !strings.Contains(doc, "()") {
			t.Errorf("%s's justification cites no line — it has to name WHERE the skipped question "+
				"is answered, not assert that it is", name)
		}
	}
}

// docBlockBefore returns the contiguous run of // comment lines immediately
// preceding the end of src.
func docBlockBefore(src string) string {
	lines := strings.Split(src, "\n")
	// The last element is the partial line the declaration starts on.
	if len(lines) > 0 {
		lines = lines[:len(lines)-1]
	}
	var out []string
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(l, "//") {
			break
		}
		out = append([]string{l}, out...)
	}
	return strings.Join(out, "\n")
}
