package shared

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"shingo/protocol"
)

// TestStatusClassesCoversAllProtocolStatuses asserts that every value
// in protocol.AllStatuses() has a corresponding .badge-<status> rule
// in shared/status-classes.css.
//
// Adding a new status to protocol/status.go without adding its CSS
// class fails this test in CI. Without the test, a missed CSS rule
// would render the new status as the neutral base .badge (still
// readable thanks to the fallback styling, but visually wrong).
//
// The check parses the CSS file literally and looks for the rule
// header `.badge-<value>` somewhere in the file (any selector that
// starts with that class, including comma-separated multi-selector
// lists for the dark theme block). The presence check is intentionally
// permissive — a rule may live under :root, [data-theme="dark"], or
// any other context.
func TestStatusClassesCoversAllProtocolStatuses(t *testing.T) {
	src := readShared(t, "status-classes.css")
	for _, s := range protocol.AllStatuses() {
		class := "badge-" + string(s)
		if !cssDeclaresClass(src, class) {
			t.Errorf("status-classes.css missing rule for .%s (protocol.Status %q)", class, s)
		}
	}
}

// TestStatusClassesNoStrayBadgeClasses asserts the inverse: no
// .badge-<x> class in the file references a status that isn't in
// protocol.AllStatuses(). Catches the case where a status was retired
// from the protocol but its CSS lingered.
//
// Allowlist:
//   - "sm" is a size modifier, not a status.
//   - "info", "warn", "muted" are non-status semantic chips — used by
//     pages like Edge's replenishment.html for source/confidence
//     labels that aren't lifecycle states. They're defined alongside
//     the status classes in shared/status-classes.css because they
//     share the .badge base style, but they're orthogonal to
//     protocol.AllStatuses().
//
// Any other non-status badge variants (e.g. role/subsystem chips)
// belong in a component-specific CSS file, not status-classes.css.
func TestStatusClassesNoStrayBadgeClasses(t *testing.T) {
	src := readShared(t, "status-classes.css")
	statusSet := make(map[string]bool)
	for _, s := range protocol.AllStatuses() {
		statusSet[string(s)] = true
	}
	allow := map[string]bool{
		"sm":    true,
		"info":  true,
		"warn":  true,
		"muted": true,
	}

	re := regexp.MustCompile(`\.badge-([a-zA-Z0-9_]+)`)
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		suffix := m[1]
		if allow[suffix] {
			continue
		}
		if !statusSet[suffix] {
			t.Errorf("status-classes.css declares .badge-%s but protocol.AllStatuses() does not contain %q", suffix, suffix)
		}
	}
}

// TestTokensCSSHasRequiredSemantic ensures the design tokens the style
// guide promises are all defined. A removed token breaks every
// component that references it; this test fails fast on regression.
//
// Color tokens must appear in BOTH :root and [data-theme="dark"]
// blocks — a missing dark-mode override means the light value bleeds
// into dark and breaks contrast. Geometry tokens only need to exist
// once (they're theme-invariant).
func TestTokensCSSHasRequiredSemantic(t *testing.T) {
	src := readShared(t, "tokens.css")
	light := extractTheme(t, src, ":root")
	dark := extractTheme(t, src, `[data-theme="dark"]`)

	colorTokens := []string{
		"--bg", "--surface", "--border",
		"--text", "--text-muted",
		"--primary", "--primary-hover",
		"--success", "--danger", "--warning", "--info",
	}
	for _, tok := range colorTokens {
		if extractTokenValue(light, tok) == "" {
			t.Errorf("tokens.css :root missing %s", tok)
		}
		if extractTokenValue(dark, tok) == "" {
			t.Errorf("tokens.css [data-theme=dark] missing %s", tok)
		}
	}

	geometryTokens := []string{"--radius", "--shadow"}
	for _, tok := range geometryTokens {
		if extractTokenValue(light, tok) == "" {
			t.Errorf("tokens.css :root missing %s", tok)
		}
	}
}

// TestTokensCSSInfoDistinctFromPrimary catches the regression Round 1
// flagged: Edge's previous dark-mode --info was the same #58a6ff as
// --primary, making info callouts indistinguishable from primary CTAs.
// The shared tokens explicitly pick a different cyan; this test pins
// the rule "they must differ" rather than the specific hexes.
func TestTokensCSSInfoDistinctFromPrimary(t *testing.T) {
	src := readShared(t, "tokens.css")
	light := extractTheme(t, src, ":root")
	dark := extractTheme(t, src, `[data-theme="dark"]`)
	for label, block := range map[string]string{"light": light, "dark": dark} {
		primary := extractTokenValue(block, "--primary")
		info := extractTokenValue(block, "--info")
		if primary == "" || info == "" {
			t.Errorf("%s theme missing --primary (%q) or --info (%q)", label, primary, info)
			continue
		}
		if strings.EqualFold(primary, info) {
			t.Errorf("%s theme: --info (%s) must differ from --primary (%s)", label, info, primary)
		}
	}
}

func cssDeclaresClass(src, class string) bool {
	// Any occurrence of `.<class>` followed by a selector terminator (space,
	// comma, brace, newline) counts. Permissive on purpose — we only need
	// to know the class is REACHABLE; the specific cascade isn't what the
	// drift test asserts.
	re := regexp.MustCompile(`\.` + regexp.QuoteMeta(class) + `[\s,{]`)
	return re.MatchString(src)
}

func extractTheme(t *testing.T, src, selector string) string {
	t.Helper()
	re := regexp.MustCompile(`(?s)` + regexp.QuoteMeta(selector) + `\s*\{(.*?)\}`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("could not find selector %q block in tokens.css", selector)
	}
	return m[1]
}

func extractTokenValue(block, name string) string {
	re := regexp.MustCompile(regexp.QuoteMeta(name) + `:\s*([^;]+);`)
	m := re.FindStringSubmatch(block)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

func readShared(t *testing.T, name string) string {
	t.Helper()
	body, err := fs.ReadFile(Files, name)
	if err != nil {
		t.Fatalf("read embedded %s: %v", name, err)
	}
	return string(body)
}
