package www

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"shingoedge/domain"
)

// board_model_keys_test.go holds the board's reads to the station's JSON.
//
// The JS tests build their own tiles, so a key renamed in Go is invisible to
// them and the board draws blanks with no error. This marshals the Go view the
// way the station endpoint does and checks every key the glass reads off a
// loader tile is one the JSON carries: the model's reads of a tile, its loader,
// a served row, a bin and the view (operator-board-model.js), and the render
// layer's and the modal's reads of a tile's loader.

// keysLandingLater are reads the model makes ahead of the key: absent until
// the item that sends it, and read as absent until then.
var keysLandingLater = map[string]string{}

// readsIn returns the distinct `<prefix>.<key>` reads in a JS source for each
// receiver name, as "<prefix>.<key>".
func readsIn(src string, receivers map[string]string) []string {
	seen := map[string]bool{}
	for re, prefix := range receivers {
		for _, m := range regexp.MustCompile(re).FindAllStringSubmatch(src, -1) {
			seen[prefix+"."+m[1]] = true
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// stripComments drops // line comments, so prose naming a key is not a read.
func stripComments(src string) string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 && !strings.Contains(line[:i], "'") {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func readJS(t *testing.T, name string) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("static", "operator-station", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return stripComments(string(src))
}

// boardFixture is a station view with every key the board may read on a
// loader tile set: a consume loader tile with a bin, a served row.
func boardFixture() domain.OperatorStationView {
	have := 7
	manifest := "{}"
	return domain.OperatorStationView{
		Nodes: []domain.StationNodeView{{
			Node:    domain.Node{ID: 1, CoreNodeName: "ULN_1", Name: "ULN_1"},
			Runtime: &domain.RuntimeState{ProcessNodeID: 1, RemainingUOPCached: 7},
			// The synthesised claim the Go tile keeps; the JSON must not carry it.
			ActiveClaim: &domain.NodeClaim{CoreNodeName: "ULN_1", Role: "consume", SwapMode: "manual_swap"},
			Loader: &domain.TileLoader{
				Role: "consume", Layout: "shared_window", Replenishment: "operator", OutboundDest: "EMPTY-SUPER",
				Parts: []domain.TileLoaderPart{{Code: "PART-A"}}, Carriers: []string{"TOTE-1"},
				LeavesBare: true, PulledDirectly: true,
				Buffer: []domain.BufferSpot{{Node: "SMN_F1", State: domain.SpotPartial}},
			},
			// An order with every field set: the station's JSON carries the keys the
			// glass reads and no other.
			Orders: []domain.Order{fullOrder()},
			BinState: &domain.NodeBinState{BinID: 3, BinLabel: "BIN-3", BinTypeCode: "TOTE-1", Bare: true,
				PayloadCode: "PART-A", UOPRemaining: 7, Manifest: &manifest, ManifestConfirmed: true, Occupied: true,
				// A held partial: its line, its arrival and its count (held_uop).
				UOPCapacity: 10, HeldSince: "2026-10-18T10:00:00Z", HeldFor: "PRESS-1"},
			BinTotes:       &domain.BinTotes{TotesOnCart: &have, TotesPerLayer: &have},
			Assignment:     &domain.TileAssignment{BinType: "TOTE-1", Layers: &have},
			BufferPartial:  &domain.BufferPartial{Node: "SMN_F1"},
			SupplyRefusals: map[string]domain.SupplyRefusal{"PART-A": {}},
			// The board's alarm sentences: a card's note.
			StrandedAlarm:      "counts are waiting for a bin",
			ContainmentFlagged: true,
			AskPending:         "A full of PART-A is asked of Core; no answer yet",
		}},
		Serves: []domain.ServedRow{{Cell: "PRESS-1", Part: "PART-A", Role: "produce", Carrier: "TOTE-1",
			HaveUOP: &have, CapacityUOP: 10, Incoming: true, Node: "PLN_1", ProcessID: 2, ProcessNodeID: 3,
			ReorderUOP: 5, Waiting: true}},
		Empties:     &domain.Empties{Group: "EMPTY-SUPER", Rows: []domain.EmptiesRow{{BinType: "TOTE-1", Have: 1, Spots: 2}}},
		Bins:        &domain.BinsKnown{Known: true},
		Containment: &domain.ContainmentKnown{Known: false},
		Catalog:     &domain.CatalogKnown{Known: false},
	}
}

func objectKeys(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal object: %v (%s)", err, raw)
	}
	return m
}

// TestStationView_ModelReadsAreWireKeys is the check: every read the glass
// makes of a loader tile is a key of the station's JSON, the loader tile
// carries no claim, and its bin carries stocked and no count.
func TestStationView_ModelReadsAreWireKeys(t *testing.T) {
	raw, err := json.Marshal(boardFixture())
	if err != nil {
		t.Fatalf("marshal view: %v", err)
	}
	view := objectKeys(t, raw)
	var nodes []json.RawMessage
	if err := json.Unmarshal(view["nodes"], &nodes); err != nil {
		t.Fatalf("nodes: %v", err)
	}
	tile := objectKeys(t, nodes[0])
	var rows []json.RawMessage
	if err := json.Unmarshal(view["serves"], &rows); err != nil {
		t.Fatalf("serves: %v", err)
	}
	keys := map[string]map[string]json.RawMessage{
		"view":   view,
		"tile":   tile,
		"loader": objectKeys(t, tile["loader"]),
		"row":    objectKeys(t, rows[0]),
		"bin":    objectKeys(t, tile["bin_state"]),
	}

	if _, ok := tile["active_claim"]; ok {
		t.Error("a loader tile's JSON carries active_claim; the board reads tile.loader")
	}
	if _, ok := keys["bin"]["uop_remaining"]; ok {
		t.Error("the station's bin_state carries uop_remaining; it carries stocked")
	}
	if string(keys["bin"]["stocked"]) != "true" {
		t.Errorf("bin_state.stocked = %s, want true for a count above 0", keys["bin"]["stocked"])
	}

	reads := readsIn(readJS(t, "operator-board-model.js"), map[string]string{
		`\btile\.([a-z_]+)`:   "tile",
		`\bloader\.([a-z_]+)`: "loader",
		`\brow\.([a-z_]+)`:    "row",
		`\bbin\.([a-z_]+)`:    "bin",
		`\bview\.([a-z_]+)`:   "view",
	})
	for _, name := range []string{"operator-render.js", "operator-modal.js", "operator-window-state.js"} {
		reads = append(reads, readsIn(readJS(t, name), map[string]string{
			`\bloader\.([a-z_]+)`:                   "loader",
			`loaderOf\([a-z]+\)\.([a-z_]+)`:         "loader",
			`\b(?:entry|n|node)\.loader\.([a-z_]+)`: "loader",
		})...)
	}
	if len(reads) < 10 {
		t.Fatalf("found %d reads; the extraction has stopped seeing the model: %v", len(reads), reads)
	}
	for _, r := range reads {
		if _, later := keysLandingLater[r]; later {
			continue
		}
		prefix, key, _ := strings.Cut(r, ".")
		if _, ok := keys[prefix][key]; !ok {
			t.Errorf("the glass reads %s, which the station's JSON does not carry", r)
		}
	}
}

// TestGlass_ReadsNoBinCount holds the station's bin count off the glass: a
// read of bin_state.uop_remaining in any of the board's modules would read a
// key the station's JSON no longer carries (the board asks stocked).
func TestGlass_ReadsNoBinCount(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("static", "operator-station", "operator*.js"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	re := regexp.MustCompile(`\b(?:bin_state|binState|bs|bin)\.uop_remaining\b`)
	checked := 0
	for _, f := range files {
		base := filepath.Base(f)
		if strings.HasSuffix(base, ".test.js") || strings.HasPrefix(base, "operator-flow") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		checked++
		if loc := re.FindString(stripComments(string(src))); loc != "" {
			t.Errorf("%s reads %s; the station's bin carries stocked, not its count", base, loc)
		}
	}
	if checked < 5 {
		t.Errorf("checked %d modules; the glob has stopped seeing the board", checked)
	}
}

// fullOrder is an order row with every field the Go struct carries set.
func fullOrder() domain.Order {
	s, n, at := "x", int64(1), time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	wait := 1
	return domain.Order{ID: 1, UUID: "u-1", OrderType: "retrieve", Status: "in_transit", ProcessNodeID: &n,
		RetrieveEmpty: true, Quantity: 1, DeliveryNode: "D", StagingNode: "S", SourceNode: "O", LoadType: "standard",
		WaybillID: &s, ExternalRef: &s, FinalCount: &n, CountConfirmed: true, ETA: &s, AutoConfirm: true,
		StagedExpireAt: &at, BinID: &n, PayloadCode: "PART-A", PayloadDesc: "d", SiblingOrderID: &n,
		QueueReason: "r", QueueCode: "c", FaultSince: &at, FaultDeadline: &at, FaultNoticeAfterS: 1,
		AuthoredBy: "a", OriginID: "o", OriginClass: "c", LaneHeld: true, StationWait: &wait, WaitKind: "k",
		ReleaseHeld: "h", DepartedAt: &at, Departed: true, CreatedAt: at, UpdatedAt: at,
		ProcessName: "p", ProcessNodeName: "pn", StationName: "sn"}
}

// stationOrderKeys are the keys of a tile's order row on the station's JSON:
// the ones the glass reads (operator-modal.js, operator-window-state.js,
// operator-render.js, operator-board-model.js, operator-state.js,
// operator-util.js). The rest of domain.Order stays on the Go tile for the
// engine and the Production page's template.
var stationOrderKeys = []string{
	"auto_confirm", "created_at", "delivery_node", "departed", "eta", "id", "lane_held", "order_type",
	"payload_code", "queue_code", "queue_reason", "sibling_order_id", "source_node", "status",
}

// TestStationView_OrderRowsCarryTheKeysTheGlassReads: a tile's order on the
// station's JSON carries exactly stationOrderKeys, and every read the glass
// makes of an order is one of them.
func TestStationView_OrderRowsCarryTheKeysTheGlassReads(t *testing.T) {
	raw, err := json.Marshal(boardFixture())
	if err != nil {
		t.Fatalf("marshal view: %v", err)
	}
	var view struct {
		Nodes []struct {
			Orders []map[string]json.RawMessage `json:"orders"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := make([]string, 0)
	for k := range view.Nodes[0].Orders[0] {
		got = append(got, k)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(stationOrderKeys, ",") {
		t.Errorf("a tile's order carries %v\nwant %v", got, stationOrderKeys)
	}
	want := map[string]bool{}
	for _, k := range stationOrderKeys {
		want[k] = true
	}
	re := `\b(?:o|order|inFlight|blocker|anchor|sibling|delivered|leftover)\.([a-z_]+)\b(?:[^(a-zA-Z_]|$)`
	reads := 0
	for _, name := range []string{"operator-modal.js", "operator-window-state.js", "operator-render.js",
		"operator-board-model.js", "operator-util.js", "operator-release.js"} {
		for _, r := range readsIn(readJS(t, name), map[string]string{re: "order"}) {
			reads++
			_, key, _ := strings.Cut(r, ".")
			if !want[key] {
				t.Errorf("%s reads order.%s, which a tile's order on the station's JSON does not carry", name, key)
			}
		}
	}
	if reads < 8 {
		t.Fatalf("found %d order reads; the extraction has stopped seeing them", reads)
	}
}
