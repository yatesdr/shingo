package www

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingocore/domain"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/payloads"
	"shingocore/store/reconciliation"
)

// count_shown_test.go — Core's server-drawn counts above a part's ratio of 1
// (owner, 2026-11-19): the whole UoP where the count stood, its parts small
// beside it; at a ratio of 1 the page as it was. One rule (countShown), and
// each page that reads it, rendered through its template.

func TestCountShown(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		count, ratio, shown int
		parts               string
	}{
		{404, 8, 50, "404 parts"},
		{803, 8, 100, "803 parts"},
		{-407, 8, -50, "-407 parts"},
		{1, 8, 0, "1 part"},
		{404, 1, 404, ""},
		{404, 0, 404, ""},
	} {
		if shown, parts := countShown(c.count, c.ratio); shown != c.shown || parts != c.parts {
			t.Errorf("countShown(%d, %d) = %d, %q; want %d, %q", c.count, c.ratio, shown, parts, c.shown, c.parts)
		}
	}
}

// The dashboard's count anomalies: each side's count, "-" when a side has
// none, and above the part's ratio of 1 its UoP with its parts beside it.
func TestDashboardCountAnomaliesInUoP(t *testing.T) {
	t.Parallel()
	n := func(v int) *int { return &v }
	for _, c := range []struct {
		n           *int
		ratio       int
		said, parts string
	}{
		{n(404), 8, "50 UoP", "404 parts"},
		{n(404), 1, "404", ""},
		{nil, 8, "-", ""},
	} {
		if said, parts := anomalyCount(c.n, c.ratio); said != c.said || parts != c.parts {
			t.Errorf("anomalyCount(%v, %d) = %q, %q; want %q, %q", c.n, c.ratio, said, parts, c.said, c.parts)
		}
	}
	row := func(ratio int) countAnomalyView {
		v := countAnomalyView{Bin: "SHG:0007", Seat: "ALN_004", Payload: "P", Class: "count differs", Open: "5m", Station: "edge.1"}
		v.Edge, v.EdgeParts = anomalyCount(n(404), ratio)
		v.Core, v.CoreParts = anomalyCount(n(392), ratio)
		return v
	}
	page := func(ratio int) string {
		return renderPage(t, "dashboard.html", map[string]any{
			"Page": "dashboard", "Health": CoreHealth{}, "Recon": &reconciliation.Summary{},
			"CountAnomalies": []countAnomalyView{row(ratio)},
		})
	}
	if out := page(8); !strings.Contains(out, `Edge <b>50 UoP</b> <span class="text-muted-xs">404 parts</span>, Core <b>49 UoP</b> <span class="text-muted-xs">392 parts</span>`) {
		t.Errorf("at 8 the anomaly does not read its UoP with its parts beside it:\n%s", anomalyLine(out))
	}
	if out := page(1); !strings.Contains(out, `Edge <b>404</b>, Core <b>392</b> <span class="text-muted-xs">edge.1`) {
		t.Errorf("at 1 the anomaly does not read as it did:\n%s", anomalyLine(out))
	}
}

func anomalyLine(out string) string {
	if i := strings.Index(out, "cs-anomalies"); i >= 0 {
		return out[i:min(len(out), i+400)]
	}
	return "(no cs-anomalies list)"
}

// Material flags' ledger: above the part's ratio of 1 the cell shows its UoP
// and the parts beside it; a part at 1, or not in the ratios, as built.
func TestMaterialFlagsLedgerInUoP(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	cap8 := 800
	rows, summary := BuildBindingRows([]domain.CarrierBinding{
		{BinID: 1, Label: "CARRIER-8", PayloadCode: "P-8", NodeName: "SMN_001", UOPRemaining: -407, UOPCapacity: &cap8},
		{BinID: 2, Label: "CARRIER-1", PayloadCode: "P-1", NodeName: "SMN_002", UOPRemaining: -10, UOPCapacity: &cap8},
	}, now, disp())
	showLedgersInUoP(rows, map[string]int{"P-8": 8})
	byLabel := map[string]BindingRow{}
	for _, r := range rows {
		byLabel[r.Label] = r
	}
	if r := byLabel["CARRIER-8"]; r.LedgerCell.Text != "-50" || r.LedgerParts != "-407 parts" || r.Ledger != -407 {
		t.Errorf("at 8: cell %q, parts %q, ledger %d; want -50, -407 parts, -407 kept", r.LedgerCell.Text, r.LedgerParts, r.Ledger)
	}
	if r := byLabel["CARRIER-1"]; r.LedgerCell.Text != "-10" || r.LedgerParts != "" {
		t.Errorf("at 1: cell %q, parts %q; want -10 and none", r.LedgerCell.Text, r.LedgerParts)
	}
	out := renderPage(t, "material-flags.html", map[string]any{
		"Page": "material-flags", "Bindings": rows, "BindingSummary": summary,
	})
	if !regexp.MustCompile(`-50\s*<span class="text-muted-xs">-407 parts</span></td>`).MatchString(out) {
		t.Errorf("rendered /material-flags does not show the ledger at 8 with its parts beside it")
	}
}

// A threshold episode's level at open and its threshold, above the part's
// ratio of 1; a kanban episode counts carriers and is left as it reads.
func TestDemandEpisodeLevelInUoP(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	thr := domain.DemandOrigin{OriginID: "o1", EpisodeKey: "k1", Kind: protocol.EpisodeKindThreshold,
		PayloadCode: "P-8", OpenedAt: now.Add(-time.Hour), OpenedTotal: 404, Threshold: 800}
	d := BuildEpisodeDetail(thr, nil, false, nil, now, disp())
	d.showInUoP(thr, 8)
	if d.OpenedTotal != 50 || d.OpenedTotalParts != "404 parts" || d.Threshold.Text != "100" || d.ThresholdParts != "800 parts" {
		t.Errorf("threshold at 8: level %d %q, threshold %q %q; want 50 404 parts, 100 800 parts",
			d.OpenedTotal, d.OpenedTotalParts, d.Threshold.Text, d.ThresholdParts)
	}
	at1 := BuildEpisodeDetail(thr, nil, false, nil, now, disp())
	at1.showInUoP(thr, 1)
	if at1.OpenedTotal != 404 || at1.OpenedTotalParts != "" || at1.Threshold.Text != "800" || at1.ThresholdParts != "" {
		t.Errorf("threshold at 1: level %d %q, threshold %q %q; want 404 and 800 as they were",
			at1.OpenedTotal, at1.OpenedTotalParts, at1.Threshold.Text, at1.ThresholdParts)
	}
	kan := thr
	kan.Kind = protocol.EpisodeKindKanban
	kd := BuildEpisodeDetail(kan, nil, false, nil, now, disp())
	kd.showInUoP(kan, 8)
	if kd.OpenedTotal != 404 || kd.OpenedTotalParts != "" {
		t.Errorf("a kanban episode counts carriers: level %d %q, want 404 unchanged", kd.OpenedTotal, kd.OpenedTotalParts)
	}
	out := renderPage(t, "demand-episode.html", map[string]any{
		"Page": "demand-episodes", "WorryAfter": "45m", "ConcernAfter": "60m", "MinExpected": 2, "OriginID": "o1", "Detail": d,
	})
	for _, want := range []string{
		`<td class="col-num tnum">50 <span class="text-muted-xs">404 parts</span></td>`,
		`100\s*<span class="text-muted-xs">800 parts</span></td>`,
	} {
		if !regexp.MustCompile(want).MatchString(out) {
			t.Errorf("rendered episode is missing %q", want)
		}
	}
}

// The Payloads table's pack: above a part's ratio of 1 the UoP a full bin
// makes, its parts beside it; a part at 1 and a kit (no ratio) as they read.
func TestPayloadsCapacityInUoP(t *testing.T) {
	t.Parallel()
	list := []*payloads.Payload{
		{ID: 1, Code: "P-8", UOPCapacity: 800},
		{ID: 2, Code: "P-1", UOPCapacity: 800},
		{ID: 3, Code: "KIT", UOPCapacity: 60},
	}
	caps := capacityInUoP(list, map[string]int{"P-8": 8})
	if c := caps[1]; c == nil || c.Shown != 100 || c.Parts != "800 parts" || caps[2] != nil || caps[3] != nil || len(caps) != 1 {
		t.Fatalf("capacityInUoP = %+v; want only P-8, 100 and 800 parts", caps)
	}
	out := renderPage(t, "payloads.html", map[string]any{
		"Page": "payloads", "Payloads": list, "BinTypes": []any{}, "CompatNodes": map[int64][]string{},
		"PayloadBinTypes": map[int64][]string{}, "CapacityInUoP": caps, "FloorMade": map[int64]*service.FloorMade{},
		"ContainmentState": map[string]*store.PayloadContainmentRow{}, "UnroutedProducers": map[string]string{},
	})
	for _, want := range []string{
		`<td>100 <span class="text-muted bin-uop-parts">800 parts</span></td>`,
		`<td>800</td>`,
		`<td>60</td>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered /payloads is missing %q", want)
		}
	}
}
