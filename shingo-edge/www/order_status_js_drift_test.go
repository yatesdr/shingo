package www

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"shingo/protocol"
)

// TestOrderStatusJSAgreesWithProtocol pins the JS-side status arrays in
// static/operator-station/order-status.js to the Go-side projectors in
// protocol/status.go. Adding a new status to the protocol map without
// updating the JS file (or vice versa) fails this test, making silent
// drift between the backend and the operator-station HMI impossible.
//
// The check parses the literal array contents from the JS file via a
// permissive regex (matches `export const NAME = ['a', 'b', ...]`) and
// compares the lex-sorted member list against the Go projector's
// equivalent. We don't run a JS engine — the JS arrays are required to
// be plain string literals, no computation, so a regex pin is enough.
func TestOrderStatusJSAgreesWithProtocol(t *testing.T) {
	path := filepath.Join("static", "operator-station", "order-status.js")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := string(body)

	cases := []struct {
		jsConst     string
		goProjector func() string
	}{
		{"TERMINAL_STATUSES", protocol.TerminalStatusSQLList},
		{"OPERATOR_VISIBLE_STATUSES", protocol.OperatorVisibleStatusSQLList},
	}

	for _, tc := range cases {
		t.Run(tc.jsConst, func(t *testing.T) {
			jsList := extractJSStatusArray(t, path, src, tc.jsConst)
			goList := splitSQLList(tc.goProjector())
			sort.Strings(jsList)
			sort.Strings(goList)
			if len(jsList) != len(goList) {
				t.Fatalf("%s: js len=%d (%v) vs go len=%d (%v)",
					tc.jsConst, len(jsList), jsList, len(goList), goList)
			}
			for i := range jsList {
				if jsList[i] != goList[i] {
					t.Errorf("%s mismatch at index %d: js=%q go=%q (js=%v go=%v)",
						tc.jsConst, i, jsList[i], goList[i], jsList, goList)
				}
			}
		})
	}
}

// TestWindowStateActiveStatusesAgreeWithProtocol pins operator-window-state.js's
// WINDOW_ACTIVE_STATUSES array against the protocol's non-terminal set.
// operator-window-state.js used to inline a narrower hand-list of "active"
// statuses than order-status.js's canonical isActive (= !terminal). Orders in
// the missing statuses (sourcing, dispatched, submitted, faulted, reshuffling)
// vanished from window cards (fell to NO DEMAND) while the operator modal still
// counted them. This test keeps the inlined copy from silently drifting again.
func TestWindowStateActiveStatusesAgreeWithProtocol(t *testing.T) {
	path := filepath.Join("static", "operator-station", "operator-window-state.js")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := string(body)

	jsList := extractJSStatusArray(t, path, src, "WINDOW_ACTIVE_STATUSES")
	goList := splitSQLList(protocol.NonTerminalStatusSQLList())
	sort.Strings(jsList)
	sort.Strings(goList)
	if len(jsList) != len(goList) {
		t.Fatalf("WINDOW_ACTIVE_STATUSES: js len=%d (%v) vs go len=%d (%v) — the inlined active list must mirror !terminal",
			len(jsList), jsList, len(goList), goList)
	}
	for i := range jsList {
		if jsList[i] != goList[i] {
			t.Errorf("WINDOW_ACTIVE_STATUSES mismatch at index %d: js=%q go=%q (js=%v go=%v)",
				i, jsList[i], goList[i], jsList, goList)
		}
	}
}

// extractJSStatusArray finds a top-level `export const NAME = [ ... ];`
// declaration and returns the string members. Fails the test if the
// declaration isn't found — that's a stronger guarantee than returning
// nil silently (a missing declaration is the drift we want to catch).
func extractJSStatusArray(t *testing.T, path, src, name string) []string {
	t.Helper()
	// Multi-line tolerant: capture everything between [ and ].
	re := regexp.MustCompile(`(?s)export\s+const\s+` + regexp.QuoteMeta(name) + `\s*=\s*\[(.*?)\]\s*;`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("could not find `export const %s = [...]` in %s", name, path)
	}
	// Inner content: 'a', 'b', ...  (whitespace and newlines allowed).
	tokenRe := regexp.MustCompile(`'([^']+)'`)
	tokens := tokenRe.FindAllStringSubmatch(m[1], -1)
	out := make([]string, 0, len(tokens))
	for _, tk := range tokens {
		out = append(out, tk[1])
	}
	return out
}

// splitSQLList undoes buildStatusSQLList's join+quote: turns
// `'a','b','c'` back into ["a","b","c"].
func splitSQLList(sqlList string) []string {
	if sqlList == "" {
		return nil
	}
	parts := strings.Split(sqlList, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, "'")
		out = append(out, p)
	}
	return out
}
