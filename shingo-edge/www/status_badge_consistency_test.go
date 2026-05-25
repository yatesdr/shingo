package www

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestEdgeStatusBadgesUseSharedClasses asserts that every status-bearing
// <span> in the edge templates that surfaces an order status uses the
// shared `badge badge-{{.Status}}` markup, not the legacy
// `<span class="status-badge">{{.Status}}</span>` pattern that ignored
// the status colorization.
//
// The test is intentionally narrow: it inspects the canonical
// order-status partials (orders-body.html, material-body.html) and
// fails if a status-bearing span lacks the badge-{{.Status}} modifier.
//
// Why: round-3 #1 regression flagged that Edge admin status badges
// previously rendered uncolored because the template emitted only
// .status-badge (the size pill, no color). Cross-surface badge
// consistency (round-3 #11 + #14) means a "delivered" order shows the
// same green-pill on Core's /orders and Edge's /orders. The drift test
// here pins that promise at CI time.
func TestEdgeStatusBadgesUseSharedClasses(t *testing.T) {
	cases := []struct {
		path  string
		want  string // substring required to be present
		veto  string // substring required to be ABSENT
		label string
	}{
		{
			path:  filepath.Join("templates", "partials", "orders-body.html"),
			want:  `class="badge badge-{{.Status}}"`,
			veto:  `class="status-badge">{{.Status}}`,
			label: "orders partial status cell",
		},
		{
			path:  filepath.Join("templates", "partials", "material-body.html"),
			want:  `class="badge badge-{{.Status}}"`,
			veto:  `class="status-badge">{{.Status}}`,
			label: "material partial per-order status",
		},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			body, err := os.ReadFile(tc.path)
			if err != nil {
				t.Fatalf("read %s: %v", tc.path, err)
			}
			src := string(body)
			if !containsCaseSensitive(src, tc.want) {
				t.Errorf("%s: expected substring %q not found — status cell is not using badge-{{.Status}} pattern", tc.path, tc.want)
			}
			if containsCaseSensitive(src, tc.veto) {
				t.Errorf("%s: forbidden legacy substring %q still present — should have been migrated", tc.path, tc.veto)
			}
		})
	}
}

func containsCaseSensitive(haystack, needle string) bool {
	return regexp.MustCompile(regexp.QuoteMeta(needle)).FindStringIndex(haystack) != nil
}
