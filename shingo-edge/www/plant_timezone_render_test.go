package www

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
	"time"

	"shingoedge/domain"
)

// The plant-local display convention, pinned in rendered MARKUP on the edge
// side — the twin of core/www's TestRenderedPagesArePlantLocal. Storage is
// UTC, the wire is RFC3339-UTC, and the server's first paint is plant-local
// and labeled, so there is no post-paint rewrite to flicker. The edge parses
// its templates FLAT (router.go: templates/*.html + partials, one set), not
// core's clone-per-page, so this harness mirrors that shape.
//
// If this regresses, edge goes back to painting UTC and letting
// convertTimestamps rewrite it — the flicker defect this work closed, and the
// drift-with-core defect underneath it (the two binaries used to render the
// same event on different clocks).
func TestRenderedPageIsPlantLocal(t *testing.T) {
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	orig := plantLocation
	plantLocation = chicago
	defer func() { plantLocation = orig }()

	tmpl := template.Must(template.New("").
		Funcs(templateFuncs()).
		ParseFS(templatesFS, "templates/*.html", "templates/partials/*.html"))

	// 14:23:01Z == 09:23 CDT. Changeover history rows run through
	// {{formatTime .StartedAt}} and {{formatTimePtr .CompletedAt}} — the two
	// paths planttime.Format serves. ActiveProcessID gates the history table.
	started := time.Date(2026, 9, 5, 14, 23, 1, 0, time.UTC)
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "changeover.html", map[string]any{
		"Page": "changeover", "ActiveProcessID": int64(1),
		"ChangeoverHistory": []domain.Changeover{
			{StartedAt: started, CompletedAt: &started},
		},
	}); err != nil {
		t.Fatalf("render changeover.html: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, `data-utc="2026-09-05T14:23:01Z"`) {
		t.Errorf("rendered page missing the RFC3339-UTC instant in data-utc")
	}
	if !strings.Contains(out, "Sep 5, 2026 09:23 CDT") {
		t.Errorf("rendered page missing plant-local labeled text %q — "+
			"regression to UTC paint would show %q", "Sep 5, 2026 09:23 CDT", "Sep 5, 2026 14:23 UTC")
	}
}

// TestHeaderCarriesPlantTZ pins the inline zone handoff: header.html writes
// window.PLANT_TZ synchronously from the server-resolved zone, so the JS
// formatters and the server agree on one clock with no fetch-flash. An empty
// or stale value here is the "two clocks on one screen" defect.
func TestHeaderCarriesPlantTZ(t *testing.T) {
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	orig := plantLocation
	plantLocation = chicago
	defer func() { plantLocation = orig }()

	// The whole set, like the sibling test above: the PLANT_TZ / SHINGO_CLOCK
	// inlines live in partials/clock-globals.html now, so header.html alone no
	// longer parses. This still pins what it always pinned — that the RENDERED
	// header carries the plant zone — and the output is unchanged by the move.
	//
	// What it cannot pin is a page that never includes the partial, which is
	// the actual defect the move exists to fix. TestEveryClockPageCarriesGlobals
	// covers that.
	tmpl := template.Must(template.New("").
		Funcs(templateFuncs()).
		ParseFS(templatesFS, "templates/*.html", "templates/partials/*.html"))

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "header", map[string]any{
		"Page": "changeover", "Authenticated": true,
	}); err != nil {
		t.Fatalf("render header.html: %v", err)
	}
	out := buf.String()

	// html/template JS-escapes the slash (\"America\\/Chicago\") — identical
	// string value to the browser, so match the escaped spelling.
	if !strings.Contains(out, `window.PLANT_TZ = "America\/Chicago";`) {
		t.Errorf("header.html missing window.PLANT_TZ = \"America/Chicago\" — the JS " +
			"formatters would fall back to the browser's local zone and disagree with the server paint")
	}
}
