package www

import (
	"io/fs"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"shingo/shared"
)

// oldConnPulseSpreadRem is how far the retired box-shadow pulse reached past
// the dot's edge at its widest (`70% { box-shadow: 0 0 0 0.6rem … }`, removed
// 2026-11-14). The compositor ring must reach exactly as far.
const oldConnPulseSpreadRem = 0.6

// TestKioskConnDotPulseIsCompositedAndHonoursReducedMotion pins the header
// connection dot on the three kiosk pages that load dashboard.css (the task
// board, the robot map, the node report).
//
// The pulse used to animate box-shadow on the dot itself, which the browser
// repaints on the main thread every frame, forever, on a wall monitor that is
// never closed. It now animates a ring on ::after with transform and opacity
// only, which the compositor runs alone. Same look: the ring starts as a disc
// the dot's size in the dot's own green at 0.6 alpha (so it hides nothing of
// the dot it sits on) and reaches the old 0.6rem spread as it fades out.
//
// And under prefers-reduced-motion the pulse is off. Before 2026-11-14 the
// reduced-motion block silenced only the robot halo, so the dot pulsed for a
// user who had asked for no motion.
func TestKioskConnDotPulseIsCompositedAndHonoursReducedMotion(t *testing.T) {
	raw, err := fs.ReadFile(staticFS, "static/dashboard.css")
	if err != nil {
		t.Fatal(err)
	}
	src := cssCommentRe.ReplaceAllString(string(raw), "")
	reduced, ok := cssBlockBody(src, "@media (prefers-reduced-motion: reduce)")
	if !ok {
		t.Fatal("dashboard.css has no prefers-reduced-motion block; this test reads nothing")
	}
	frames, ok := cssBlockBody(src, "@keyframes dash-pulse")
	if !ok {
		t.Fatal("dashboard.css has no @keyframes dash-pulse; this test reads nothing")
	}
	top := cssTopLevel(src)

	// Only the ring animates, never the dot's own box.
	var runners []string
	for _, r := range cssRules(top) {
		if strings.Contains(r.decls["animation"], "dash-pulse") || r.decls["animation-name"] == "dash-pulse" {
			runners = append(runners, r.selector)
		}
	}
	if len(runners) != 1 || runners[0] != ".dash-conn-ok::after" {
		t.Errorf("dash-pulse runs on %q, want only .dash-conn-ok::after", runners)
	}
	dot := cssRuleDecls(t, top, ".dash-conn-ok")
	for _, p := range []string{"animation", "box-shadow"} {
		if v, has := dot[p]; has {
			t.Errorf(".dash-conn-ok declares %s: %s; the dot's own box must not pulse", p, v)
		}
	}

	// Every keyframe moves only what the compositor can run alone.
	kf := map[string]map[string]string{}
	for _, f := range cssRules(frames) {
		kf[f.selector] = f.decls
		for p := range f.decls {
			if p != "transform" && p != "opacity" {
				t.Errorf("dash-pulse %s animates %s: only transform and opacity skip the main-thread repaint", f.selector, p)
			}
		}
	}

	// The same look as the box-shadow it replaced.
	ring := cssRuleDecls(t, top, ".dash-conn-ok::after")
	ringRGB, ringAlpha := cssRGBA(t, ring["background"])
	dotRGB, err := shared.ParseHexColor(dot["background"])
	if err != nil {
		t.Fatalf(".dash-conn-ok background %q: %v", dot["background"], err)
	}
	if ringRGB != dotRGB || ringAlpha != 0.6 {
		t.Errorf("ring is %v at %.2f alpha, want the dot's own %v at 0.6 (the old shadow's start), "+
			"so the ring drawn over the dot changes none of its pixels", ringRGB, ringAlpha, dotRGB)
	}
	width := cssRem(t, cssRuleDecls(t, top, ".dash-conn")["width"])
	if got := cssScale(t, kf["0%"]["transform"]); got != 1 || kf["0%"]["opacity"] != "1" {
		t.Errorf("0%%: scale %v opacity %q, want the ring at the dot's size, fully on", got, kf["0%"]["opacity"])
	}
	reach := (cssScale(t, kf["70%"]["transform"]) - 1) * width / 2
	if math.Abs(reach-oldConnPulseSpreadRem) > 0.001 || kf["70%"]["opacity"] != "0" {
		t.Errorf("70%%: the ring reaches %.4frem past the dot at opacity %q, want the old %.1frem, faded out",
			reach, kf["70%"]["opacity"], oldConnPulseSpreadRem)
	}
	if kf["100%"]["opacity"] != "0" {
		t.Errorf("100%%: opacity %q, want 0 (the old pulse is invisible from 70%% to the loop)", kf["100%"]["opacity"])
	}

	// Reduced motion stops it.
	if rm := cssRuleDecls(t, reduced, ".dash-conn-ok::after"); rm["animation"] != "none" && rm["display"] != "none" {
		t.Errorf("under prefers-reduced-motion .dash-conn-ok::after has animation %q display %q, want either none",
			rm["animation"], rm["display"])
	}
}

var (
	cssCommentRe = regexp.MustCompile(`(?s)/\*.*?\*/`)
	cssRuleRe    = regexp.MustCompile(`([^{}]+)\{([^{}]*)\}`)
	cssRGBARe    = regexp.MustCompile(`^rgba\((\d+),\s*(\d+),\s*(\d+),\s*([0-9.]+)\)$`)
	cssScaleRe   = regexp.MustCompile(`^scale\(([0-9.]+)\)$`)
)

type cssRule struct {
	selector string
	decls    map[string]string
}

// cssBlockBody returns what sits between the braces of the first block whose
// prelude is header.
func cssBlockBody(src, header string) (string, bool) {
	at := strings.Index(src, header)
	if at < 0 {
		return "", false
	}
	start, end, ok := cssBlockSpan(src, at)
	if !ok {
		return "", false
	}
	return src[start:end], true
}

// cssBlockSpan finds the first brace block at or after from: the index just
// inside its opening brace and the index of its matching closing brace.
func cssBlockSpan(src string, from int) (start, end int, ok bool) {
	open := strings.IndexByte(src[from:], '{')
	if open < 0 {
		return 0, 0, false
	}
	start = from + open + 1
	depth := 1
	for i := start; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return start, i, true
			}
		}
	}
	return 0, 0, false
}

// cssTopLevel is src with every at-rule block cut out, leaving the plain rules.
func cssTopLevel(src string) string {
	var b strings.Builder
	for {
		at := strings.IndexByte(src, '@')
		if at < 0 {
			b.WriteString(src)
			return b.String()
		}
		b.WriteString(src[:at])
		_, end, ok := cssBlockSpan(src, at)
		if !ok {
			return b.String()
		}
		src = src[end+1:]
	}
}

func cssRules(block string) []cssRule {
	var out []cssRule
	for _, m := range cssRuleRe.FindAllStringSubmatch(block, -1) {
		d := map[string]string{}
		for _, decl := range strings.Split(m[2], ";") {
			p, v, ok := strings.Cut(decl, ":")
			if ok {
				d[strings.TrimSpace(p)] = strings.TrimSpace(v)
			}
		}
		out = append(out, cssRule{selector: strings.TrimSpace(m[1]), decls: d})
	}
	return out
}

func cssRuleDecls(t *testing.T, block, selector string) map[string]string {
	t.Helper()
	for _, r := range cssRules(block) {
		if r.selector == selector {
			return r.decls
		}
	}
	t.Fatalf("dashboard.css has no %s rule where this test reads it", selector)
	return nil
}

func cssRGBA(t *testing.T, v string) (shared.RGB, float64) {
	t.Helper()
	m := cssRGBARe.FindStringSubmatch(v)
	if m == nil {
		t.Fatalf("%q is not an rgba() this test reads", v)
	}
	n := func(s string) float64 { f, _ := strconv.ParseFloat(s, 64); return f }
	return shared.RGB{R: n(m[1]), G: n(m[2]), B: n(m[3])}, n(m[4])
}

func cssScale(t *testing.T, v string) float64 {
	t.Helper()
	m := cssScaleRe.FindStringSubmatch(v)
	if m == nil {
		t.Fatalf("transform %q is not a scale() this test reads", v)
	}
	f, _ := strconv.ParseFloat(m[1], 64)
	return f
}

func cssRem(t *testing.T, v string) float64 {
	t.Helper()
	f, err := strconv.ParseFloat(strings.TrimSuffix(v, "rem"), 64)
	if err != nil || !strings.HasSuffix(v, "rem") {
		t.Fatalf("width %q is not in rem", v)
	}
	return f
}
