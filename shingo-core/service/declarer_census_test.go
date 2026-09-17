package service

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// declarer_census_test.go — who is allowed to say a person declared a reset.
//
// WHAT THE COMPILER ALREADY DOES, so this does not: bumpEpoch takes a
// protocol.Declarer with no default, so a new reset path cannot omit the
// question. The build fails on it. A census repeating that would pin nothing.
//
// WHAT IT CANNOT DO is notice that a new site answered DeclaredByPerson. That
// answer is the one with a blast radius: it tells the station it may bind an
// empty slot to the carrier the message names, and every path where that is
// wrong is wrong in the same way — the carrier has already been driven away,
// and the press's next parts are charged to it until something lands. The four
// machine paths all reach bumpEpoch through the same helpers as the two human
// ones, so copying a call and keeping its declarer is a one-line change with
// no compile error and no failing test behind it.
//
// So the pin is the SET, not the count of sites: a person declares a reset at
// the two admin doors and nowhere else.

// personDeclarationSites is every non-test site in this module that may
// announce a reset as a person's declaration, by module-relative path, with
// how many times.
//
// Both are on the bin detail modal — an operator with the bin in front of them
// typing what it now holds, or saying it is now empty. That is what makes the
// declaration true, and it is the property a new entry has to be able to claim.
var personDeclarationSites = map[string]int{
	"www/bin_actions.go": 2, // binLoadPayload, binClear
}

func TestDeclaredByPerson_OnlyAtTheAdminDoors(t *testing.T) {
	t.Parallel()
	root := ".."
	found := map[string]int{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Vendored and build output carry no doors of ours. The root is
			// exempt from the dotted-name rule: it is spelled "..", which
			// starts with a dot, and skipping it walks nothing — a census that
			// reads no files reports no violations.
			if path == root {
				return nil
			}
			if name := d.Name(); name == "vendor" || name == "node_modules" || strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			return nil
		}
		// Non-test sources only. A test naming a person is a test describing
		// this behaviour, not a door the plant can reach.
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if n := strings.Count(string(src), "protocol.DeclaredByPerson"); n > 0 {
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				// A path the walk produced that will not relativise against the
				// walk's own root is not a file to skip quietly: every later
				// comparison is by module-relative path, so dropping it here
				// would exempt that file from the census.
				return fmt.Errorf("relativise %s against %s: %w", path, root, relErr)
			}
			found[filepath.ToSlash(rel)] = n
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v (this test is the only thing holding the person-declaration set "+
			"to the code; if the module moved, repoint it rather than deleting it)", root, err)
	}

	for _, path := range sortedKeys(found) {
		want, allowed := personDeclarationSites[path]
		switch {
		case !allowed:
			t.Errorf("%s declares a reset as a person (%d site(s)) and is not in "+
				"personDeclarationSites.\n\nA person's declaration lets the station BIND an empty "+
				"slot to the carrier this names. That is right when somebody is standing at the "+
				"bin saying what is in it, and wrong everywhere else: an announcement Core "+
				"generated from a carrier's own lifecycle routinely lands after a robot has "+
				"lifted that carrier, and binding there charges the press's next parts to a bin "+
				"that has left.\n\nIf this site really is a person at a door, add it here with "+
				"what makes the declaration true. If it is machinery, it wants "+
				"protocol.DeclaredByLifecycle.", path, found[path])
		case found[path] != want:
			t.Errorf("%s has %d person declarations, pinned at %d. A site was added or removed; "+
				"check the new one is a door where somebody is looking at the carrier, then "+
				"update this number.", path, found[path], want)
		}
	}
	for _, path := range sortedKeys(personDeclarationSites) {
		if _, ok := found[path]; !ok {
			t.Errorf("personDeclarationSites names %s, which declares no reset as a person. "+
				"If the door moved, repoint this; if it is gone, drop the entry — a stale "+
				"allowlist entry is a hole the next copy-paste lands in.", path)
		}
	}
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
