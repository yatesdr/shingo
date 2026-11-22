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

	"shingo/protocol"
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
	if _, err := db.EnrollEdge(uid, ""); err != nil {
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

// TestEdgesPage_MarksAReleaseBelowTheFloor: each station's row shows the
// release its last register carried; one older than protocol.FloorEdge is
// marked below the floor, and the floor's own release and a version that
// names no release (a dev build) are shown as they are, unmarked. Core Health
// names each active station below the floor by its uid and release
// (edgesBelowFloor), and no other.
func TestEdgesPage_MarksAReleaseBelowTheFloor(t *testing.T) {
	t.Parallel()
	h, db := testHandlersForRendering(t)
	for uid, version := range map[string]string{"stn-old": "v0.16.0", "stn-at-floor": protocol.FloorEdge, "stn-dev": "dev"} {
		_, err := db.EnrollEdge(uid, "")
		testutil.MustNoErr(t, err, "enroll "+uid)
		_, err = db.RegisterEdge(uid, "host-"+uid, "inst-"+uid, version, "", sql.NullInt64{})
		testutil.MustNoErr(t, err, "register "+uid)
	}

	body := renderEdgesPage(t, h)
	row := func(uid string) string {
		r := regexp.MustCompile(`(?s)<tr data-uid="` + uid + `".*?</tr>`).FindString(body)
		if r == "" {
			t.Fatalf("no row for %s", uid)
		}
		return r
	}
	if r := row("stn-old"); !strings.Contains(r, "<code>v0.16.0</code>") || !strings.Contains(r, "edge-below-floor") {
		t.Errorf("a station on v0.16.0 is not shown below the floor (%s):\n%s", protocol.FloorEdge, r)
	}
	if r := row("stn-at-floor"); !strings.Contains(r, "<code>"+protocol.FloorEdge+"</code>") || strings.Contains(r, "edge-below-floor") {
		t.Errorf("a station on the floor's own release is not shown, or is marked below it:\n%s", r)
	}
	if r := row("stn-dev"); !strings.Contains(r, "<code>dev</code>") || strings.Contains(r, "edge-below-floor") {
		t.Errorf("a dev build is not shown as it is, or is marked below the floor:\n%s", r)
	}
	if !strings.Contains(body, ">Version</th>") {
		t.Error("the table has no Version column")
	}

	want := []string{"station stn-old runs v0.16.0, older than " + protocol.FloorEdge + ", the oldest Edge this Core supports"}
	if got := h.edgesBelowFloor(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("health's below-floor sentences = %q, want %q", got, want)
	}
}

// TestEdgesPage_WhatAStaleStationsRowOffers: the actions of the row of a stale
// station and of an active one: Remove station is offered on the stale one
// alone.
func TestEdgesPage_WhatAStaleStationsRowOffers(t *testing.T) {
	t.Parallel()
	h, db := testHandlersForRendering(t)
	for _, uid := range []string{"stn-live", "stn-gone"} {
		testutil.MustNoErr(t, enrollAndRegister(db, uid, "host-"+uid, sql.NullInt64{Int64: 1, Valid: true}), "seed "+uid)
	}
	_, err := db.Exec(`UPDATE edge_registry SET status = $1 WHERE station_uid = $2`, "stale", "stn-gone")
	testutil.MustNoErr(t, err, "one goes stale")

	body := renderEdgesPage(t, h)
	button := regexp.MustCompile(`<button[^>]*>([^<]*)</button>`)
	for uid, want := range map[string]string{"stn-live": "Rename", "stn-gone": "Rename, Remove station"} {
		row := regexp.MustCompile(`(?s)<tr data-uid="` + uid + `".*?</tr>`).FindString(body)
		var got []string
		for _, m := range button.FindAllStringSubmatch(row, -1) {
			got = append(got, strings.TrimSpace(m[1]))
		}
		if strings.Join(got, ", ") != want {
			t.Errorf("%s offers %q, want %q", uid, got, want)
		}
	}
}

// TestEdgesRemoveDoor: the removal door. With no session the router refuses it
// before the handler. The server, not the page, holds "stale only": an active
// station is refused and keeps its row and its claims. A stale one loses its
// claims mirror, the report rows of the processes that cleared, and its row;
// the removal is audited, and what else names the station stays. A process
// another station reported last is that station's and stays.
func TestEdgesRemoveDoor(t *testing.T) {
	t.Parallel()
	h, db := testHandlersForRendering(t)
	const live, gone = "stn-live", "stn-gone"
	for _, uid := range []string{live, gone} {
		testutil.MustNoErr(t, enrollAndRegister(db, uid, "host-"+uid, sql.NullInt64{Int64: 1, Valid: true}), "seed "+uid)
	}
	for _, q := range []string{
		`UPDATE edge_registry SET status = 'stale' WHERE station_uid = 'stn-gone'`,
		`INSERT INTO process_styles (process_id, style_id, station_uid) VALUES ('P-GONE', 'S', 'stn-gone'), ('P-LIVE', 'S', 'stn-live')`,
		`INSERT INTO style_claims (process_id, style_id, core_node_name, role, swap_mode)
			VALUES ('P-GONE', 'S', 'N1', 'consume', 'sequential'), ('P-LIVE', 'S', 'N1', 'consume', 'sequential')`,
		`INSERT INTO process_positions (process_id, core_node_name) VALUES ('P-GONE', 'W1'), ('P-LIVE', 'W2')`,
		`INSERT INTO plant_claims_reports (process_id, station_uid, digest, received_at) VALUES
			('P-GONE', 'stn-gone', 'g', now()), ('P-GONE', 'stn-live', 'l', now()),
			('P-LIVE', 'stn-gone', 'g', now()), ('P-LIVE', 'stn-live', 'l', now())`,
		`INSERT INTO demand_registry (station_uid, core_node_name, role, payload_code) VALUES ('stn-gone', 'W1', 'produce', 'PART')`,
	} {
		_, err := db.Exec(q)
		testutil.MustNoErr(t, err, "seed: "+q)
	}

	rec := httptest.NewRecorder()
	realRouterFor(t, h).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/edges/remove?uid="+gone, nil))
	if rec.Code == http.StatusOK || rec.Code == http.StatusNotFound {
		t.Fatalf("a removal with no session = %d, want the auth refusal", rec.Code)
	}

	remove := func(uid string) int {
		rec := httptest.NewRecorder()
		h.apiEdgeRemove(rec, httptest.NewRequest(http.MethodPost, "/api/edges/remove?uid="+uid, nil))
		return rec.Code
	}
	count := func(q string) int {
		var n int
		testutil.MustNoErr(t, db.QueryRow(q).Scan(&n), q)
		return n
	}
	if code := remove(live); code != http.StatusConflict {
		t.Fatalf("removing an active station = %d, want 409", code)
	}
	if n := count(`SELECT count(*) FROM style_claims`) + count(`SELECT count(*) FROM edge_registry`); n != 4 {
		t.Fatalf("a refused removal left %d claims and stations, want all 4", n)
	}

	if code := remove(gone); code != http.StatusOK {
		t.Fatalf("removing a stale station = %d, want 200", code)
	}
	for what, c := range map[string]struct {
		q    string
		want int
	}{
		"its row":                         {`SELECT count(*) FROM edge_registry WHERE station_uid = 'stn-gone'`, 0},
		"the row of the other station":    {`SELECT count(*) FROM edge_registry WHERE station_uid = 'stn-live'`, 1},
		"its styles":                      {`SELECT count(*) FROM process_styles WHERE process_id = 'P-GONE'`, 0},
		"its claims":                      {`SELECT count(*) FROM style_claims WHERE process_id = 'P-GONE'`, 0},
		"its positions":                   {`SELECT count(*) FROM process_positions WHERE process_id = 'P-GONE'`, 0},
		"its reports":                     {`SELECT count(*) FROM plant_claims_reports WHERE station_uid = 'stn-gone'`, 0},
		"a report of a cleared process":   {`SELECT count(*) FROM plant_claims_reports WHERE process_id = 'P-GONE'`, 0},
		"a report of a process that kept": {`SELECT count(*) FROM plant_claims_reports WHERE process_id = 'P-LIVE'`, 1},
		"the claims of the other":         {`SELECT count(*) FROM style_claims WHERE process_id = 'P-LIVE'`, 1},
		"the positions of the other":      {`SELECT count(*) FROM process_positions WHERE process_id = 'P-LIVE'`, 1},
		"its demand registry row":         {`SELECT count(*) FROM demand_registry WHERE station_uid = 'stn-gone'`, 1},
		"the audit row":                   {`SELECT count(*) FROM audit_log WHERE entity_type = 'edge' AND action = 'removed'`, 1},
	} {
		if got := count(c.q); got != c.want {
			t.Errorf("after the removal %s: %d rows, want %d", what, got, c.want)
		}
	}
	if code := remove(gone); code != http.StatusConflict {
		t.Errorf("removing it again = %d, want 409", code)
	}
}
