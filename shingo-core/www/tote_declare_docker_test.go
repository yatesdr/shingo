//go:build docker

package www

import (
	"encoding/json"
	"html/template"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"shingo/protocol/debuglog"
	"shingo/protocol/testutil"
	"shingocore/config"
	"shingocore/engine"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// tote_declare_docker_test.go — the height a person declares with the CLEAR
// (BinClearRequest.Layers), and the tote numbers on the node-bins answer.
//
// Core writes layers × the cart's own type's totes per layer to
// bins.totes_on_cart, after the clear and any stamp, in the clear's
// transaction, with a totes_declared ledger row. A type that says no totes per
// layer gets no write. A CLEAR that declares nothing is the clear it always was.

func totesOnCart(t *testing.T, db *store.DB, binID int64) *int {
	t.Helper()
	var v *int
	testutil.MustNoErr(t, db.QueryRow(`SELECT totes_on_cart FROM bins WHERE id=$1`, binID).Scan(&v), "read totes_on_cart")
	return v
}

// declaredRows is the totes_declared ledger rows for a bin, detail decoded.
func declaredRows(t *testing.T, db *store.DB, binID int64) []map[string]any {
	t.Helper()
	rows, err := db.Query(`SELECT detail FROM bin_uop_ledger WHERE bin_id=$1 AND op='totes_declared' ORDER BY id`, binID)
	testutil.MustNoErr(t, err, "read ledger")
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		testutil.MustNoErr(t, rows.Scan(&raw), "scan ledger")
		var d map[string]any
		testutil.MustNoErr(t, json.Unmarshal(raw, &d), "decode detail")
		out = append(out, d)
	}
	testutil.MustNoErr(t, rows.Err(), "ledger")
	return out
}

func toteType(t *testing.T, db *store.DB, code string, perLayer *int) *bins.BinType {
	t.Helper()
	bt := &bins.BinType{Code: code, TotesPerLayer: perLayer}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type "+code)
	return bt
}

// fullOfTypeAt is a loaded bin of type bt at a node of its own.
func fullOfTypeAt(t *testing.T, db *store.DB, sd *testdb.StandardData, bt *bins.BinType, node string) *bins.Bin {
	t.Helper()
	n := &nodes.Node{Name: node, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(n), "create "+node)
	b := testdb.CreateBinAtNode(t, db, sd.Payload.Code, n.ID, node+"-CART")
	_, err := db.Exec(`UPDATE bins SET bin_type_id=$2 WHERE id=$1`, b.ID, bt.ID)
	testutil.MustNoErr(t, err, "type the cart")
	return b
}

func clearAt(t *testing.T, h *Handlers, body map[string]any) int {
	t.Helper()
	return postJSON(t, h.apiBinClear, "/api/telemetry/bin-clear", body).Code
}

func TestBinClear_DeclaresTheHeight(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	eight, five := 8, 5
	tote := toteType(t, db, "TD-TOTE", &eight)
	kd := toteType(t, db, "TD-KD", nil)

	t.Run("a declared 2 on 8 a layer is 16, with a ledger row", func(t *testing.T) {
		b := fullOfTypeAt(t, db, sd, tote, "TD-N-16")
		if code := clearAt(t, h, map[string]any{"node_name": "TD-N-16", "layers": 2}); code != http.StatusOK {
			t.Fatalf("clear status %d", code)
		}
		if got := totesOnCart(t, db, b.ID); got == nil || *got != 16 {
			t.Errorf("totes_on_cart = %v, want 16", deref(got))
		}
		want := []map[string]any{{"layers": float64(2), "totes_per_layer": float64(8), "totes_on_cart": float64(16)}}
		if got := declaredRows(t, db, b.ID); !reflect.DeepEqual(got, want) {
			t.Errorf("totes_declared rows = %v, want %v", got, want)
		}
	})

	t.Run("0 layers declares tops & bottoms", func(t *testing.T) {
		b := fullOfTypeAt(t, db, sd, tote, "TD-N-BASE")
		if code := clearAt(t, h, map[string]any{"node_name": "TD-N-BASE", "layers": 0}); code != http.StatusOK {
			t.Fatalf("clear status %d", code)
		}
		if got := totesOnCart(t, db, b.ID); got == nil || *got != 0 {
			t.Errorf("totes_on_cart = %v, want 0", deref(got))
		}
	})

	t.Run("a type with no totes per layer: no write, no ledger row", func(t *testing.T) {
		b := fullOfTypeAt(t, db, sd, kd, "TD-N-KD")
		if code := clearAt(t, h, map[string]any{"node_name": "TD-N-KD", "layers": 2}); code != http.StatusOK {
			t.Fatalf("clear status %d", code)
		}
		if got := totesOnCart(t, db, b.ID); got != nil {
			t.Errorf("totes_on_cart = %d, want unknown", *got)
		}
		if got := declaredRows(t, db, b.ID); len(got) != 0 {
			t.Errorf("totes_declared rows = %v, want none", got)
		}
	})

	t.Run("re-typed by the CLEAR, the declare reads the new type", func(t *testing.T) {
		b := fullOfTypeAt(t, db, sd, kd, "TD-N-RETYPE")
		if code := clearAt(t, h, map[string]any{"node_name": "TD-N-RETYPE", "bin_type_code": "TD-TOTE", "layers": 3}); code != http.StatusOK {
			t.Fatalf("clear status %d", code)
		}
		if got := totesOnCart(t, db, b.ID); got == nil || *got != 24 {
			t.Errorf("totes_on_cart = %v, want 24 (3 layers of the new type's 8)", deref(got))
		}
	})

	// SEND ON at stage 2: the cart stands as its bare marker and the CLEAR
	// stamps its carrier back inside the transaction. The marker is given a
	// number of its own here so a declare that read the type before the stamp
	// would write 10, not 16.
	t.Run("the declare after a stage-2 stamp reads the carrier's number, not the marker's", func(t *testing.T) {
		markerID, err := db.EnsureBareMarker(tote.ID)
		testutil.MustNoErr(t, err, "the carrier's marker")
		_, err = db.Exec(`UPDATE bin_types SET totes_per_layer=$2 WHERE id=$1`, markerID, five)
		testutil.MustNoErr(t, err, "give the marker a number")
		n := &nodes.Node{Name: "TD-N-SENDON", Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(n), "node")
		bare := &bins.Bin{BinTypeID: markerID, Label: "TD-BARE-1", NodeID: &n.ID, Status: "available"}
		testutil.MustNoErr(t, db.CreateBin(bare), "the bare cart")

		if code := clearAt(t, h, map[string]any{"node_name": "TD-N-SENDON", "layers": 2}); code != http.StatusOK {
			t.Fatalf("clear status %d", code)
		}
		after, err := db.GetBin(bare.ID)
		testutil.MustNoErr(t, err, "read the cart")
		if after.BinTypeID != tote.ID {
			t.Fatalf("SEND ON stamped type %d, want the carrier %d", after.BinTypeID, tote.ID)
		}
		if got := totesOnCart(t, db, bare.ID); got == nil || *got != 16 {
			t.Errorf("totes_on_cart = %v, want 16 (2 layers of the carrier's 8, not the marker's 5)", deref(got))
		}
	})

	t.Run("a negative height is refused before anything is written", func(t *testing.T) {
		b := fullOfTypeAt(t, db, sd, tote, "TD-N-NEG")
		if code := clearAt(t, h, map[string]any{"node_name": "TD-N-NEG", "layers": -1}); code != http.StatusBadRequest {
			t.Fatalf("clear status %d, want 400", code)
		}
		after, err := db.GetBin(b.ID)
		testutil.MustNoErr(t, err, "read the cart")
		if after.PayloadCode == "" {
			t.Error("the refused CLEAR emptied the cart")
		}
	})
}

// countingHandlers is a Handlers whose engine reads and writes through a
// counting store and is not started, so nothing in the background adds to the
// count. Fixtures go through the returned plain store.
func countingHandlers(t *testing.T) (*Handlers, *store.DB, *store.QueryCounter) {
	t.Helper()
	db, dbcfg := testdb.OpenWithConfig(t)
	cdb, counter, err := store.OpenCounting(dbcfg)
	testutil.MustNoErr(t, err, "open counting db")
	t.Cleanup(func() { cdb.Close() })
	eng := engine.New(engine.Config{AppConfig: config.Defaults(), DB: cdb, Fleet: simulator.New(), LogFunc: t.Logf})
	hub := NewEventHub()
	hub.Start()
	t.Cleanup(func() { hub.Stop() })
	dbgLog, err := debuglog.New(64, nil)
	testutil.MustNoErr(t, err, "debug log")
	return &Handlers{engine: eng, orchestration: eng, sessions: newSessionStore("test-secret"),
		tmpls: make(map[string]*template.Template), eventHub: hub, debugLog: dbgLog}, db, counter
}

// binRow is the bin's row, whole.
func wholeBinRow(t *testing.T, db *store.DB, id int64) map[string]any {
	t.Helper()
	var raw []byte
	testutil.MustNoErr(t, db.QueryRow(`SELECT to_jsonb(b) FROM bins b WHERE id=$1`, id).Scan(&raw), "read bin row")
	var row map[string]any
	testutil.MustNoErr(t, json.Unmarshal(raw, &row), "decode bin row")
	return row
}

func changedCols(before, after map[string]any) []string {
	var out []string
	for k, v := range after {
		if !reflect.DeepEqual(before[k], v) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// TestBinClear_WithoutLayersIsTodaysClear: a plant that sets no tote numbers
// sends no layers (the Edge posts them only for a type with totes per layer),
// and its CLEAR is the clear it always was: the same columns change as before
// the tote numbers (TestEmptyingStatements_ColumnLists' list without
// totes_on_cart, which a cart with no number does not see move) and no ledger
// row is added. Declaring layers adds the declare's one statement, and its
// ledger row only where the type says how many totes a layer holds. The
// counts are logged for the report; the assertion is on the statements.
func TestBinClear_WithoutLayersIsTodaysClear(t *testing.T) {
	t.Parallel()
	h, db, counter := countingHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	eight := 8
	tote := toteType(t, db, "TC-TOTE", &eight)

	clear := func(body map[string]any) []string {
		t.Helper()
		counter.Reset()
		if code := clearAt(t, h, body); code != http.StatusOK {
			t.Fatalf("clear %v: status %d", body, code)
		}
		return counter.Statements()
	}

	plainAt := &nodes.Node{Name: "TC-N-PLAIN", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(plainAt), "create TC-N-PLAIN")
	plain := testdb.CreateBinAtNode(t, db, sd.Payload.Code, plainAt.ID, "TC-PLAIN")
	before := wholeBinRow(t, db, plain.ID)
	base := clear(map[string]any{"node_name": "TC-N-PLAIN"})
	wantCols := []string{"delta_epoch", "loaded_at", "manifest", "manifest_confirmed", "payload_code",
		"uop_remaining", "updated_at"}
	if got := changedCols(before, wholeBinRow(t, db, plain.ID)); !reflect.DeepEqual(got, wantCols) {
		t.Errorf("a CLEAR without layers changed %v, want %v", got, wantCols)
	}
	if got := declaredRows(t, db, plain.ID); len(got) != 0 {
		t.Errorf("a CLEAR without layers wrote totes_declared rows %v", got)
	}
	for _, s := range base {
		if strings.Contains(s, "totes_on_cart = $2") {
			t.Errorf("a CLEAR without layers ran the declare: %.80s", s)
		}
	}
	t.Logf("CLEAR, no layers: %d statements", len(base))

	kdCart := fullOfTypeAt(t, db, sd, toteType(t, db, "TC-KD", nil), "TC-N-KD")
	kd := clear(map[string]any{"node_name": "TC-N-KD", "layers": 2})
	t.Logf("CLEAR, layers on a type with no totes per layer: %d statements", len(kd))
	if len(kd) != len(base)+1 {
		t.Errorf("layers on a type with no totes per layer: %d statements, want %d (the clear's and the declare's one)",
			len(kd), len(base)+1)
	}
	if got := totesOnCart(t, db, kdCart.ID); got != nil {
		t.Errorf("KD cart totes_on_cart = %d, want unknown", *got)
	}

	fullOfTypeAt(t, db, sd, tote, "TC-N-TOTE")
	declared := clear(map[string]any{"node_name": "TC-N-TOTE", "layers": 2})
	t.Logf("CLEAR, layers on a tote type: %d statements", len(declared))
	if len(declared) != len(base)+2 {
		t.Errorf("layers on a tote type: %d statements, want %d (the clear's, the declare and its ledger row)",
			len(declared), len(base)+2)
	}
}

// TestNodeBins_ToteNumbers: the bins feed's row carries the cart's count of
// totes and its type's totes per layer when they are known, 0 included, and
// leaves both keys out when not. From the bin read the row already makes: no
// statement is added.
func TestNodeBins_ToteNumbers(t *testing.T) {
	t.Parallel()
	_, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	eight := 8
	tote := toteType(t, db, "TN-TOTE", &eight)

	b16 := fullOfTypeAt(t, db, sd, tote, "TN-N-16")
	_, err := db.Exec(`UPDATE bins SET totes_on_cart=16 WHERE id=$1`, b16.ID)
	testutil.MustNoErr(t, err, "stand 16 totes")
	b0 := fullOfTypeAt(t, db, sd, tote, "TN-N-0")
	_, err = db.Exec(`UPDATE bins SET totes_on_cart=0 WHERE id=$1`, b0.ID)
	testutil.MustNoErr(t, err, "stand tops & bottoms")
	fullOfTypeAt(t, db, sd, tote, "TN-N-UNKNOWN")

	for _, tc := range []struct {
		node      string
		totes     any
		hasTotes  bool
		perLayer  any
		hasPerLay bool
	}{
		{"TN-N-16", float64(16), true, float64(8), true},
		{"TN-N-0", float64(0), true, float64(8), true},
		{"TN-N-UNKNOWN", nil, false, float64(8), true},
	} {
		row := nodeBinsRow(t, db, tc.node)
		got, ok := row["totes_on_cart"]
		if ok != tc.hasTotes || got != tc.totes {
			t.Errorf("%s totes_on_cart = %v (present %v), want %v (present %v)", tc.node, got, ok, tc.totes, tc.hasTotes)
		}
		got, ok = row["totes_per_layer"]
		if ok != tc.hasPerLay || got != tc.perLayer {
			t.Errorf("%s totes_per_layer = %v (present %v), want %v", tc.node, got, ok, tc.perLayer)
		}
	}
}
