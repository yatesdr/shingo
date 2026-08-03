package protocol_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"shingo/protocol"
)

// A subject in an inbound list with no handler registered for it is a boot
// failure, not a test failure. Both composition roots walk their own list and
// log.Fatalf on the first one they cannot serve, so the process refuses to
// start — at a plant, that means the line is down and the reason is a line in
// a journal.
//
// THIS TEST EXISTS BECAUSE THAT NEARLY SHIPPED. The loader cutover deleted the
// Edge's below-threshold handler and its registration and left the subject in
// EdgeInboundSubjects(). Every suite stayed green: nothing outside main()
// touches that list, so no test could see it. The Edge would have refused to
// boot on the first deploy.
//
// CORE ALREADY HAS THE REAL VERSION of this check: its router construction was
// deliberately lifted out of main() into a file a test can call, and that test
// builds the router and asks it. The Edge's construction is still inline in
// main() and cannot be called, so it has no equivalent — which is why the
// mistake was possible on that side and not the other.
//
// So this covers the Edge only, and it reads main.go as DATA rather than
// building anything. It is the cheap version of the check Core has, and the
// right fix is to do for the Edge what was done for Core: lift the router
// construction somewhere callable and then delete this in favour of the real
// thing. That is a refactor of the composition root, not a line, and it is
// recorded as an open item rather than smuggled in here.

func TestEveryInboundSubjectIsRegisteredAtItsCompositionRoot(t *testing.T) {
	t.Parallel()
	cases := []struct {
		side     string
		root     string
		subjects []string
		// optional names a subject whose registration is legitimately
		// conditional. The boot check gates on the same condition.
		optional map[string]string
	}{
		{
			side:     "Edge",
			root:     "../shingo-edge/cmd/shingoedge/main.go",
			subjects: protocol.EdgeInboundSubjects(),
			optional: map[string]string{
				"SubjectCountGroupCommand": "countgroup is an optional feature; " +
					"the boot check skips it when no handler is configured",
			},
		},
	}

	for _, c := range cases {
		src, err := os.ReadFile(c.root)
		if err != nil {
			t.Fatalf("%s: read %s: %v (if the composition root moved, repoint this test "+
				"rather than deleting it — it is the only thing between a deleted handler "+
				"and a box that will not start)", c.side, c.root, err)
		}
		text := string(src)

		for _, subject := range c.subjects {
			name := constNameFor(t, subject)
			if _, ok := c.optional[name]; ok {
				continue
			}
			// A registration names the constant, not the wire string.
			registered := regexp.MustCompile(`RegisterSubject\([^)]*\bprotocol\.` + regexp.QuoteMeta(name) + `\b`).
				MatchString(text)
			if !registered {
				t.Errorf("%s lists %s (%q) as inbound but %s registers no handler for it.\n\n"+
					"This is not a style problem. The composition root walks its own inbound "+
					"list at boot and log.Fatalf's on the first subject it cannot serve, so "+
					"the process will not start.\n\n"+
					"If the subject is retired, remove it from the list in the SAME change "+
					"that removes the handler. If it is new, register it.",
					c.side, name, subject, c.root)
			}
		}
	}
}

// constNameFor maps a wire value back to its Subject* constant name by reading
// types.go, so the test reports the identifier a person would grep for rather
// than the string.
func constNameFor(t *testing.T, wire string) string {
	t.Helper()
	src, err := os.ReadFile("types.go")
	if err != nil {
		t.Fatalf("read types.go: %v", err)
	}
	re := regexp.MustCompile(`(?m)^\s*(?:const\s+)?(Subject\w+)\s*=\s*"` + regexp.QuoteMeta(wire) + `"`)
	if m := re.FindStringSubmatch(string(src)); m != nil {
		return m[1]
	}
	// A few subjects are declared in their own files; fall back to a scan.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read protocol dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		b, err := os.ReadFile(e.Name())
		if err != nil {
			continue
		}
		if m := re.FindStringSubmatch(string(b)); m != nil {
			return m[1]
		}
	}
	t.Fatalf("no Subject* constant declares the wire value %q — the inbound list and the "+
		"constants have diverged", wire)
	return ""
}
