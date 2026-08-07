package www

import (
	"os"
	"strings"
	"testing"
)

func readSourceFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// funcBody returns a function's CODE — from its opening line to the next
// top-level closing brace, with comment lines stripped.
//
// THE STRIPPING IS LOAD-BEARING, NOT TIDINESS. A comment explaining why a call
// was REMOVED necessarily names the thing that was removed, so a scan over raw
// text reports the explanation as the offence. The first draft of this helper
// did exactly that: it failed apiRobotMoveTo for "calling" the two functions
// its comment says it deliberately does not call.
func funcBody(t *testing.T, src, sig string) string {
	t.Helper()
	i := strings.Index(src, sig)
	if i < 0 {
		t.Fatalf("function %q not found", sig)
	}
	rest := src[i:]
	if j := strings.Index(rest, "\n}\n"); j >= 0 {
		rest = rest[:j]
	}
	var code []string
	for _, ln := range strings.Split(rest, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "//") {
			continue
		}
		code = append(code, ln)
	}
	return strings.Join(code, "\n")
}
