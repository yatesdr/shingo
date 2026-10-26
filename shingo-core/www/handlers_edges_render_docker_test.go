//go:build docker

package www

import (
	"database/sql"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/store"
)

// TestEdgesPage_RowsLineUpWithHostAndSchema renders /edges through the
// real handler and pins the table's shape, the Host column, and the station
// schema column.
//
// THE SHAPE FIRST. The Schema column once REPLACED the Host cell instead of
// joining it: eight headers over seven cells, every column from Host rightward
// sitting under the wrong heading, and the station's hostname gone from the
// page. A header/cell count per row is the check that catches that class.
//
// THEN THE FACTS. Each station's hostname, and its reported schema version (vN,
// or "unknown" when no register carried one).
func TestEdgesPage_RowsLineUpWithHostAndSchema(t *testing.T) {
	t.Parallel()
	h, db := testHandlersForRendering(t)

	// One station that registered with a schema version, one that never did.
	testutil.MustNoErr(t, enrollAndRegister(db, "stn-with-schema", "edge-host-alpha", sql.NullInt64{Int64: 1, Valid: true}),
		"seed station with a schema version")
	testutil.MustNoErr(t, enrollAndRegister(db, "stn-no-schema", "edge-host-beta", sql.NullInt64{}),
		"seed station without a schema version")

	body := renderEdgesPage(t, h)

	table := regexp.MustCompile(`(?s)<table class="table" id="edges-table">.*?</table>`).FindString(body)
	if table == "" {
		t.Fatalf("no edges table on the page:\n%s", body)
	}
	heads := len(regexp.MustCompile(`<th[ >]`).FindAllString(table, -1))
	rows := regexp.MustCompile(`(?s)<tr data-uid="[^"]*".*?</tr>`).FindAllString(table, -1)
	if len(rows) != 2 {
		t.Fatalf("edge rows = %d, want 2", len(rows))
	}
	for _, row := range rows {
		if cells := len(regexp.MustCompile(`<td[ >]`).FindAllString(row, -1)); cells != heads {
			t.Errorf("row has %d cells under %d headers — columns are shifted:\n%s", cells, heads, row)
		}
	}

	for _, want := range []string{"edge-host-alpha", "edge-host-beta"} {
		if !strings.Contains(table, want) {
			t.Errorf("hostname %q is not in the table (Host column missing)", want)
		}
	}
	if !strings.Contains(table, ">Host</th>") || !strings.Contains(table, ">Schema</th>") {
		t.Errorf("table is missing its Host or Schema header:\n%s", table)
	}
	if !strings.Contains(table, ">v1</td>") {
		t.Error("the reporting station's schema version v1 is not rendered")
	}
	if !strings.Contains(table, ">unknown</span>") {
		t.Error("the station with no reported schema version does not read \"unknown\"")
	}
	coreWant := fmt.Sprintf("Core schema v%d", store.LatestMigrationVersion())
	if !strings.Contains(body, coreWant) {
		t.Errorf("Core's own schema version (%q) is not on the page", coreWant)
	}
}

func enrollAndRegister(db *store.DB, uid, hostname string, schemaVersion sql.NullInt64) error {
	if _, err := db.EnrollEdge(uid, "", uid); err != nil {
		return fmt.Errorf("enroll %s: %w", uid, err)
	}
	if _, err := db.RegisterEdge(uid, hostname, "inst-"+uid, "test", "", schemaVersion); err != nil {
		return fmt.Errorf("register %s: %w", uid, err)
	}
	return nil
}

// renderEdgesPage runs the real handler with the template set loaded as
// router.go loads it (same shape as renderOrdersPage).
func renderEdgesPage(t *testing.T, h *Handlers) string {
	t.Helper()
	if len(h.tmpls) == 0 {
		base := template.New("").Funcs(templateFuncs(h.engine.NodeService()))
		base = template.Must(base.ParseFS(templateFS, "templates/layout.html", "templates/partials/*.html"))
		pages, err := fs.Glob(templateFS, "templates/*.html")
		testutil.MustNoErr(t, err, "glob templates")
		for _, p := range pages {
			name := p[len("templates/"):]
			if name == "layout.html" {
				continue
			}
			clone := template.Must(base.Clone())
			h.tmpls[name] = template.Must(clone.ParseFS(templateFS, p))
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/edges", nil)
	rec := httptest.NewRecorder()
	h.handleEdgesAdmin(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("edges page status = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}
