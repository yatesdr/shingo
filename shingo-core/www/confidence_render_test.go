package www

import (
	"math"
	"testing"
	"text/template"
)

// The localization-confidence tile's two template helpers. Both are pure
// functions of one float, so they are tested directly out of the FuncMap
// rather than through a rendered page.

func confidenceFuncs(t *testing.T) (f2 func(float64) string, band func(float64) string) {
	t.Helper()
	fm := templateFuncs(nil)
	f2, ok := fm["f2"].(func(float64) string)
	if !ok {
		t.Fatal("f2 missing from the template FuncMap")
	}
	band, ok = fm["confidenceBand"].(func(float64) string)
	if !ok {
		t.Fatal("confidenceBand missing from the template FuncMap")
	}
	return f2, band
}

// NEGATIVE ZERO IS REAL DATA, NOT A HYPOTHETICAL. Springfield AMR-04 reported
// -0 fifteen times in the first two minutes of collection, while driving and
// on task. IEEE preserves the sign of zero, so an unguarded "%.2f" prints
// "-0.00" — which reads as a broken display rather than as the genuine
// near-total loss of localization it represents.
func TestF2_NormalisesNegativeZero(t *testing.T) {
	f2, _ := confidenceFuncs(t)

	negZero := math.Copysign(0, -1)
	if !math.Signbit(negZero) {
		t.Fatal("fixture is wrong: that is not a negative zero")
	}
	if got := f2(negZero); got != "0.00" {
		t.Errorf("f2(-0) = %q, want \"0.00\"", got)
	}
	if got := f2(0); got != "0.00" {
		t.Errorf("f2(+0) = %q, want \"0.00\"", got)
	}
}

// Two decimals, exactly as the vendor publishes the figure — no rescaling to
// a percentage, so an operator comparing this tile against RoboShop sees the
// same number.
func TestF2_PrintsTwoDecimals(t *testing.T) {
	f2, _ := confidenceFuncs(t)
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{0.9288, "0.93"},
		{0.5, "0.50"},
		{1, "1.00"},
		{0.6117, "0.61"},
	} {
		if got := f2(tc.in); got != tc.want {
			t.Errorf("f2(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The band cuts are the VENDOR'S operator thresholds (rds-user-manual.pdf:
// >0.8 green, >0.3 yellow, else red), so this page and the fleet manager
// agree about the same number instead of offering second opinions.
func TestConfidenceBand_MatchesVendorThresholds(t *testing.T) {
	_, band := confidenceFuncs(t)
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{1.00, "chip-ok"},
		{0.80, "chip-ok"},
		{0.7999, "chip-near"},
		{0.30, "chip-near"},
		{0.2999, "chip-below"},
		{0.00, "chip-below"},
		{math.Copysign(0, -1), "chip-below"}, // a lost robot is not healthy
	} {
		if got := band(tc.in); got != tc.want {
			t.Errorf("confidenceBand(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The helpers must be registered under the names robots.html calls them by; a
// missing FuncMap entry is a template parse error at render time, which is a
// blank robots page rather than a build failure.
func TestConfidenceHelpersAreRegistered(t *testing.T) {
	tmpl := template.New("t").Funcs(templateFuncs(nil))
	if _, err := tmpl.Parse(`{{f2 .C}}{{confidenceBand .C}}`); err != nil {
		t.Fatalf("robots.html's helpers do not parse: %v", err)
	}
}
