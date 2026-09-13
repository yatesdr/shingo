package shared

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// identity_hue_drift_test.go — the robot identity hues, and the style guide's
// account of them.
//
// The three tokens are the only saturated colours on a cell picture or a plant
// map that are not a status, and the guide devotes a section to why (§ "Robot
// identity hues"). Two things can go wrong quietly:
//
//   - A hue moves in tokens.css and the guide keeps quoting the old one. The
//     guide is where the next person looks before touching them, and a guide
//     that quotes a dead value is worse than one that says nothing.
//   - A hue is added to tokens.css and never written down, which is how
//     --chart-grid ended up derived from the text ramp: a token with no entry
//     in the guide is a token nobody knows the rule for.
//
// The per-surface COPIES (--os-r1 / --os-r2 / --os-station in operator.css)
// are held to these by shingo-edge/www/operator_tokens_drift_test.go. This
// test is the other axis: the master against the prose.

// identityHues is the set, and the guide's table must carry every one.
var identityHues = []string{"--robot-1", "--robot-2", "--station"}

func TestIdentityHuesAreDeclaredOnce(t *testing.T) {
	src := readShared(t, "tokens.css")
	root := extractTheme(t, src, ":root")
	dark := extractTheme(t, src, `[data-theme="dark"]`)
	for _, tok := range identityHues {
		if extractTokenValue(root, tok) == "" {
			t.Errorf("tokens.css :root declares no %s", tok)
		}
		// THEME-INVARIANT ON PURPOSE, and this is the assertion that says so.
		// A robot's colour is its identity: the same robot on a light admin
		// page and a dark HMI is the same robot, and a per-theme value would
		// mean the teal leg on the desktop and the teal leg on the station
		// were two different teals nobody had reconciled.
		if v := extractTokenValue(dark, tok); v != "" {
			t.Errorf("tokens.css overrides %s in the dark theme (%s); an identity hue is one value, both themes", tok, v)
		}
	}
	if extractTokenValue(root, "--robot-1") == extractTokenValue(root, "--robot-2") {
		t.Error("Robot 1 and Robot 2 share a value — two robots on one picture would be indistinguishable")
	}
}

// TestStyleGuideQuotesTheIdentityHues holds the guide's token block to
// tokens.css, value for value.
func TestStyleGuideQuotesTheIdentityHues(t *testing.T) {
	src := readShared(t, "tokens.css")
	root := extractTheme(t, src, ":root")
	guide := readStyleGuide(t)
	for _, tok := range identityHues {
		want := extractTokenValue(root, tok)
		if want == "" {
			t.Fatalf("tokens.css declares no %s — this test would pass vacuously", tok)
		}
		// The guide names it, in its token block and in its own table.
		hits := regexp.MustCompile(regexp.QuoteMeta(tok)).FindAllString(guide, -1)
		if len(hits) < 2 {
			t.Errorf("docs/ui-style-guide.md mentions %s %d time(s); it belongs in the shared token block AND in the identity-hue table",
				tok, len(hits))
		}
		if !regexp.MustCompile(regexp.QuoteMeta(want)).MatchString(guide) {
			t.Errorf("docs/ui-style-guide.md never quotes %s's value %s. Whichever is right, both move together.", tok, want)
		}
	}
}

// readStyleGuide reads the guide off disk. It is not embedded — it is
// documentation, not a served asset — so the path is relative to this package
// and up two levels to the repo root.
func readStyleGuide(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "docs", "ui-style-guide.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

// ── the ink on a fill, and the scrim (U9 fix-up) ─────────────────────────────
//
// Two tokens with the same failure mode as the hues above, arrived at from the
// other direction: they did not exist, so every component that needed them
// wrote the literal. White on --accent-solid was `#fff` in five places across
// three files, and the modal backdrop was rgba(0,0,0,.5) on Edge and
// rgba(0,0,0,.45) on Core — two scrims nobody had decided were different.
//
// What this holds is the pair of properties that make them worth having:
//
//   - the token exists and the guide quotes its value, exactly as for a hue;
//   - the components that used to carry the literal now read the token, on
//     BOTH admin surfaces. That is the load-bearing half. A token nothing
//     references is a token the next literal ignores.

// surfaceInkTokens is the pair, with whether the dark theme is allowed to
// override it. The ink is not: the fill under it is theme-invariant, so
// the white on it is one value. The scrim is: the page it darkens is.
var surfaceInkTokens = []struct {
	name     string
	perTheme bool
}{
	{"--on-accent-solid", false},
	{"--scrim", true},
}

func TestSurfaceInkTokensAreDeclared(t *testing.T) {
	src := readShared(t, "tokens.css")
	root := extractTheme(t, src, ":root")
	dark := extractTheme(t, src, `[data-theme="dark"]`)
	for _, tok := range surfaceInkTokens {
		if extractTokenValue(root, tok.name) == "" {
			t.Errorf("tokens.css :root declares no %s", tok.name)
		}
		v := extractTokenValue(dark, tok.name)
		switch {
		case tok.perTheme && v == "":
			t.Errorf("tokens.css does not override %s in the dark theme; the scrim darkens a page whose colour changes, so its value has to change with it", tok.name)
		case !tok.perTheme && v != "":
			t.Errorf("tokens.css overrides %s in the dark theme (%s); it sits on a theme-invariant fill, so it is one value in both", tok.name, v)
		}
	}
}

func TestStyleGuideQuotesTheSurfaceInkTokens(t *testing.T) {
	src := readShared(t, "tokens.css")
	root := extractTheme(t, src, ":root")
	guide := readStyleGuide(t)
	for _, tok := range surfaceInkTokens {
		want := extractTokenValue(root, tok.name)
		if want == "" {
			t.Fatalf("tokens.css declares no %s — this test would pass vacuously", tok.name)
		}
		if !regexp.MustCompile(regexp.QuoteMeta(tok.name)).MatchString(guide) {
			t.Errorf("docs/ui-style-guide.md never names %s; a token with no entry in the guide is a token nobody knows the rule for", tok.name)
		}
		if !regexp.MustCompile(regexp.QuoteMeta(want)).MatchString(guide) {
			t.Errorf("docs/ui-style-guide.md never quotes %s's value %s. Whichever is right, both move together.", tok.name, want)
		}
	}
}

// TestSurfaceInkTokensAreReadByBothAdminSurfaces is the half that keeps the
// literal from coming back. Each entry is a file and the declarations that
// must reference the token rather than spell the colour out.
func TestSurfaceInkTokensAreReadByBothAdminSurfaces(t *testing.T) {
	//
	// MATCHED AS A DECLARATION, not as bytes. These were whitespace-exact
	// strings ("background: var(--scrim);"), so reformatting the stylesheet —
	// two spaces, a line break, a shorthand — failed a test about whether the
	// token is read at all. The property and the var() are the rule.
	decl := func(prop, tok string) *regexp.Regexp {
		return regexp.MustCompile(regexp.QuoteMeta(prop) + `\s*:\s*var\(\s*` + regexp.QuoteMeta(tok) + `\s*[,)]`)
	}
	for _, c := range []struct {
		path string
		want *regexp.Regexp
	}{
		{filepath.Join("..", "shingo-edge", "www", "static", "css", "shingoedge.css"), decl("background", "--scrim")},
		{filepath.Join("..", "shingo-edge", "www", "static", "css", "shingoedge.css"), decl("color", "--on-accent-solid")},
		{filepath.Join("..", "shingo-core", "www", "static", "style.css"), decl("background", "--scrim")},
		{filepath.Join("..", "shingo-core", "www", "static", "style.css"), decl("color", "--on-accent-solid")},
		// processes-desktop.css HAS NO SCRIM ROW, and that is the point: the
		// Processes page's #pd-scrim used to carry a .pd-scrim rule that was
		// .modal-overlay's declarations under a second name. It carries the
		// base class now, so the one scrim declaration on this surface is
		// shingoedge.css's above — pinned there, once.
		{filepath.Join("..", "shingo-edge", "www", "static", "css", "processes-desktop.css"), decl("color", "--on-accent-solid")},
	} {
		body, err := os.ReadFile(c.path)
		if err != nil {
			t.Fatalf("read %s: %v", c.path, err)
		}
		if !c.want.Match(body) {
			t.Errorf("%s has no declaration matching %v — the literal it replaced is the thing this token exists to prevent", c.path, c.want)
		}
	}
}

// TestTheStationPictureCarriesNoLiteralColour: the exception is retired.
//
// flow-picture.css carried four declarations with a literal colour, moved
// verbatim out of operator.css by U9a's extraction, and the guide named them
// as the one known exception "until the station reads shared/tokens.css".
// operator-display.html links it now, so the condition is met and the
// literals are tokens — which is not cosmetic: the near-white two of them used
// is --text-strong's DARK value, and on a light Processes page it put the
// position name in near-white on a white card.
//
// The test survives inverted. A literal creeping back into the one stylesheet
// two surfaces share is exactly the thing worth catching.
func TestTheStationPictureCarriesNoLiteralColour(t *testing.T) {
	path := filepath.Join("..", "shingo-edge", "www", "static", "operator-station", "flow-picture.css")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	// Comments are stripped first: this file's own header explains which
	// colours it used to spell out, and a rule that reported its own prose
	// would be one nobody could satisfy.
	body = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAll(body, []byte(" "))
	lit := regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b|rgba?\(`)
	var offenders []string
	for _, line := range strings.Split(string(body), "\n") {
		if lit.MatchString(line) {
			offenders = append(offenders, strings.TrimSpace(line))
		}
	}
	if len(offenders) > 0 {
		t.Errorf("flow-picture.css spells out %d colour(s). Both surfaces load shared/tokens.css now, "+
			"so there is no reason left for a literal here — add a token:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}
