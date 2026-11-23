package reuse

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// testMainDecl is a TestMain declaration as go test's testmain generator finds
// it: a func named TestMain in one of the package's _test.go files (internal or
// external test package).
var testMainDecl = regexp.MustCompile(`(?m)^func[ \t]+TestMain[ \t]*\(`)

// hasTestMain is whether the package in dir (go test runs a binary in its
// package directory) declares a TestMain, and the _test.go files it read to
// decide. A TestMain's work before it calls m.Run is invisible to the test log,
// so such a binary is keyed on the whole tree.
//
// Read from the source, not the binary: go test links a test binary with
// -s -w unless -c or a profile flag needs the symbols (go1.25.6, the test
// command sets OmitDebug on the test main), so it has no symbol table on any
// OS. Every _test.go file is read whatever its build constraints, and a match
// in a comment counts: both err towards "has one".
// A directory that cannot be read counts as having one.
func hasTestMain(dir string) (has bool, why string, files []string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return true, "package directory unreadable (" + err.Error() + "): counted as a TestMain", nil
	}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		files = append(files, p)
		src, err := os.ReadFile(p)
		switch {
		case err != nil:
			has, why = true, e.Name()+" unreadable: counted as a TestMain"
		case !has && testMainDecl.Match(src):
			has, why = true, "TestMain in "+e.Name()
		}
	}
	return has, why, files
}
