//go:build docker

package uop_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/plantclaims"
	"shingocore/uop"
)

// produce_writers_effects_docker_test.go — every write the two produce writers
// make, row by row.
//
// The produce tick's bind (ApplyBinUOPDelta's identity binding) and produce
// finalize (RecordProducedBin, and RecordProducedBinFromTemplate through it)
// both stamp a payload onto a carrier, and both run on a carrier a dispatched
// order holds. These pins record exactly what each one leaves behind: the bin
// columns that changed (old and new value), every bin_uop_ledger row it
// appended, and every outbox message it queued for the bin. A change to how
// either path is built must leave this list byte-for-byte as it is.
//
// Values that differ run to run are rendered symbolically: a timestamp within
// an hour of the test's clock is "<now>", and ids are the fixture's names for
// them. Every other value is the literal the database holds.
//
// The carriers stand at a seat one station claims, so a write that starts a new
// generation queues its announcement to that station and the pin sees it.

const (
	effectsTopic   = "shingo.dispatch.effects-test"
	effectsStation = "edge-pw.line"
)

// fixedProducedAt is a produced_at far from the test clock, so a loaded_at
// taken from it renders as its literal value rather than as "<now>".
const fixedProducedAt = "2026-01-02T03:04:05Z"

type effectsFixture struct {
	db       *store.DB
	mani     *service.BinManifestService
	svc      *uop.InventoryDeltaService
	nodeID   int64
	fitsID   int64
	foreign  int64
	declared string            // payload with bin-type rules: only fitsID may carry it
	sparse   string            // payload with no bin-type rules
	names    map[string]string // "<class>:<id>" -> name; see effectsIDClass
}

func setupEffectsFixture(t *testing.T) *effectsFixture {
	t.Helper()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	seat := &nodes.Node{Name: "PW-SEAT", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(seat), "create the seat")
	ann := service.EpochAnnounce{Topic: effectsTopic, CoreStation: "core.test"}
	mani := service.NewBinManifestService(db, ann)
	f := &effectsFixture{
		db: db, mani: mani, svc: uop.NewInventoryDeltaService(db, mani, ann),
		nodeID: seat.ID, declared: "PWDECL", sparse: "PWSPARSE",
		names: map[string]string{},
	}
	_, err := db.ReplacePlantClaimsFromStation("PW-PROC", effectsStation,
		[]plantclaims.StyleRow{{ProcessID: "PW-PROC", StyleID: "PW-RUN", IsActive: true}},
		[]plantclaims.ClaimRow{{ProcessID: "PW-PROC", StyleID: "PW-RUN", CoreNodeName: seat.Name,
			Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeSequential, PayloadCode: f.declared, UOPCapacity: 500}},
		nil, 0)
	testutil.MustNoErr(t, err, "claim the seat")
	fits := &bins.BinType{Code: "PW-FITS"}
	testutil.MustNoErr(t, db.CreateBinType(fits), "create PW-FITS")
	foreign := &bins.BinType{Code: "PW-FOREIGN"}
	testutil.MustNoErr(t, db.CreateBinType(foreign), "create PW-FOREIGN")
	f.fitsID, f.foreign = fits.ID, foreign.ID
	payloadWithTwoLines(t, db, f.declared)
	payloadWithTwoLines(t, db, f.sparse)
	p, err := db.GetPayloadByCode(f.declared)
	testutil.MustNoErr(t, err, "read declared payload")
	testutil.MustNoErr(t, db.SetPayloadBinTypes(p.ID, []int64{fits.ID}), "declare bin types")
	f.name("node", f.nodeID, "<node>")
	f.name("type", fits.ID, "<type:fits>")
	f.name("type", foreign.ID, "<type:foreign>")
	return f
}

// heldBin creates a carrier of binTypeID at the seat, optionally
// labelled payloadCode with uop aboard, claimed by a dispatched order.
func (f *effectsFixture) heldBin(t *testing.T, label string, binTypeID int64, payloadCode string, uop int) int64 {
	t.Helper()
	b := &bins.Bin{BinTypeID: binTypeID, Label: label, NodeID: &f.nodeID, Status: "available"}
	testutil.MustNoErr(t, f.db.CreateBin(b), "create bin "+label)
	if payloadCode != "" {
		testutil.MustNoErr(t, f.db.SetBinManifest(b.ID, `{"items":[{"part_number":"OLD-PN"}]}`, payloadCode, uop),
			"label bin "+label)
		testutil.MustNoErr(t, f.db.ConfirmBinManifest(b.ID, fixedProducedAt), "confirm bin "+label)
	}
	o := testdb.CreateOrder(t, f.db, func(o *orders.Order) { o.Status = protocol.StatusDispatched })
	testdb.ClaimBinForTest(t, f.db, b.ID, o.ID)
	f.name("bin", b.ID, "<bin>")
	f.name("order", o.ID, "<order>")
	return b.ID
}

// stamp sets a timestamp column on a bin to a fixed past instant, so a write
// that keeps the first stamp (COALESCE) shows the literal and one that
// overwrites it shows "<now>".
func (f *effectsFixture) stamp(t *testing.T, binID int64, col string) {
	t.Helper()
	if _, err := f.db.Exec(`UPDATE bins SET `+col+` = $2::timestamptz WHERE id=$1`, binID, "2026-02-03T04:05:06Z"); err != nil {
		t.Fatalf("stamp %s on bin %d: %v", col, binID, err)
	}
}

// effectsIDClass is what each id-bearing key names, so ids from different
// tables that happen to share a value are told apart.
var effectsIDClass = map[string]string{
	"id": "bin", "bin_id": "bin", "node_id": "node", "claimed_by": "order", "order_id": "order",
	"bin_type_id": "type", "loader_id": "loader",
}

func (f *effectsFixture) name(class string, id int64, name string) {
	f.names[fmt.Sprintf("%s:%d", class, id)] = name
}

// normEffect renders v with run-to-run values made symbolic (see the file
// comment).
func (f *effectsFixture) normEffect(key string, v any, now time.Time) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, c := range x {
			out[k] = f.normEffect(k, c, now)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, c := range x {
			out[i] = f.normEffect(key, c, now)
		}
		return out
	case string:
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999-07:00"} {
			if ts, err := time.Parse(layout, x); err == nil {
				if d := ts.Sub(now); d < time.Hour && d > -time.Hour {
					return "<now>"
				}
				return ts.UTC().Format(time.RFC3339)
			}
		}
		return x
	case float64:
		if class, ok := effectsIDClass[key]; ok {
			if n, ok := f.names[fmt.Sprintf("%s:%d", class, int64(x))]; ok {
				return n
			}
			return "<" + class + "?>"
		}
		return x
	}
	return v
}

func (f *effectsFixture) render(key string, v any) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(f.normEffect(key, v, time.Now())); err != nil {
		return fmt.Sprintf("<unmarshalable %v>", err)
	}
	return strings.TrimSpace(b.String())
}

func (f *effectsFixture) binRow(t *testing.T, binID int64) map[string]any {
	t.Helper()
	var raw []byte
	testutil.MustNoErr(t, f.db.QueryRow(`SELECT to_jsonb(b) FROM bins b WHERE b.id=$1`, binID).Scan(&raw), "read bin row")
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode bin row: %v", err)
	}
	return m
}

func (f *effectsFixture) maxID(t *testing.T, table string) int64 {
	t.Helper()
	var id int64
	testutil.MustNoErr(t, f.db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM `+table).Scan(&id), "max id "+table)
	return id
}

// effectsOf runs write and returns every effect it had on binID, in a fixed
// order: the changed bin columns (sorted), then the ledger rows and the outbox
// messages in the order they were written.
func (f *effectsFixture) effectsOf(t *testing.T, binID int64, write func() error) []string {
	t.Helper()
	before := f.binRow(t, binID)
	ledgerFrom, outboxFrom := f.maxID(t, "bin_uop_ledger"), f.maxID(t, "outbox")
	testutil.MustNoErr(t, write(), "the write under test")
	after := f.binRow(t, binID)

	var out []string
	keys := make([]string, 0, len(after))
	for k := range after {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !reflect.DeepEqual(before[k], after[k]) {
			out = append(out, fmt.Sprintf("bins.%s: %s -> %s", k, f.render(k, before[k]), f.render(k, after[k])))
		}
	}
	out = append(out, f.ledgerSince(t, binID, ledgerFrom)...)
	return append(out, f.outboxSince(t, binID, outboxFrom)...)
}

func (f *effectsFixture) ledgerSince(t *testing.T, binID, from int64) []string {
	t.Helper()
	rows, err := f.db.Query(`SELECT jsonb_build_object(
			'before_uop', before_uop, 'after_uop', after_uop, 'op', op, 'source', source,
			'order_id', order_id, 'payload_code', payload_code, 'actor', actor,
			'metadata', metadata, 'node_id', node_id, 'station', station,
			'detail', detail, 'loader_id', loader_id, 'reason', reason)
		FROM bin_uop_ledger WHERE bin_id=$1 AND id>$2 ORDER BY id`, binID, from)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan ledger row: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("decode ledger row: %v", err)
		}
		out = append(out, "ledger: "+f.render("", m))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate ledger: %v", err)
	}
	return out
}

func (f *effectsFixture) outboxSince(t *testing.T, binID, from int64) []string {
	t.Helper()
	rows, err := f.db.Query(`SELECT topic, msg_type, station_id, payload FROM outbox WHERE id>$1 ORDER BY id`, from)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var topic, msgType, station string
		var raw []byte
		if err := rows.Scan(&topic, &msgType, &station, &raw); err != nil {
			t.Fatalf("scan outbox row: %v", err)
		}
		var env protocol.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("decode envelope: %v", err)
		}
		var data protocol.Data
		if err := env.DecodePayload(&data); err != nil {
			t.Fatalf("decode data payload: %v", err)
		}
		var body map[string]any
		if err := json.Unmarshal(data.Body, &body); err != nil {
			t.Fatalf("decode outbox body: %v", err)
		}
		if id, ok := body["bin_id"].(float64); !ok || int64(id) != binID {
			continue
		}
		out = append(out, fmt.Sprintf("outbox: topic=%s type=%s station=%s env=%s src=%s dst=%s subject=%s body=%s",
			topic, msgType, station, env.Type, f.render("", env.Src), f.render("", env.Dst), data.Subject,
			f.render("", body)))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate outbox: %v", err)
	}
	return out
}

func assertEffects(t *testing.T, name string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s: effects changed.\n got:\n  %s\n want:\n  %s", name,
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// TestProduceTickBind_EffectsOntoAHeldCarrier pins the produce tick's bind,
// both arms, on carriers a dispatched order holds.
func TestProduceTickBind_EffectsOntoAHeldCarrier(t *testing.T) {
	t.Parallel()
	f := setupEffectsFixture(t)
	tick := func(binID int64, code string, delta int) func() error {
		return func() error {
			return f.svc.ApplyBinUOPDelta(testStation, makeBinDelta(binID, code, delta, 1, protocol.ReasonProduceTick))
		}
	}

	cases := []struct {
		name  string
		setup func() int64
		write func(int64) func() error
		want  []string
	}{
		{"first tick on a blank carrier",
			func() int64 { return f.heldBin(t, "PW-T-BLANK", f.fitsID, "", 0) },
			func(id int64) func() error { return tick(id, f.declared, 3) },
			[]string{
				`bins.manifest: null -> {"items":[{"part_number":"PWDECL-PN-Z"},{"part_number":"PWDECL-PN-A"}]}`,
				`bins.payload_code: "" -> "PWDECL"`,
				`bins.uop_remaining: 0 -> 3`,
				`ledger: {"actor":"ALN_001","after_uop":0,"before_uop":0,"detail":null,"loader_id":null,"metadata":{"old_payload":"","sequence_id":1},"node_id":null,"op":"payload_bound_first_delta","order_id":null,"payload_code":"PWDECL","reason":"","source":"service/inventory_delta_service.go:firstDeltaBind","station":""}`,
				`ledger: {"actor":"ALN_001","after_uop":3,"before_uop":0,"detail":null,"loader_id":null,"metadata":{"bin_epoch":1,"delta":3,"healed":0,"reason":"produce_tick","sequence_id":1,"window_end":"<now>","window_start":"<now>","wire_delta":3,"wire_epoch":0},"node_id":"<node>","op":"bin_uop_delta","order_id":null,"payload_code":"PWDECL","reason":"produce_tick","source":"service/inventory_delta_service.go","station":""}`,
			}},
		{"first tick over a stale label at zero",
			func() int64 { return f.heldBin(t, "PW-T-STALE", f.fitsID, "PWOLD", 0) },
			func(id int64) func() error { return tick(id, f.declared, 2) },
			[]string{
				`bins.manifest: {"items":[{"part_number":"OLD-PN"}]} -> {"items":[{"part_number":"PWDECL-PN-Z"},{"part_number":"PWDECL-PN-A"}]}`,
				`bins.payload_code: "PWOLD" -> "PWDECL"`,
				`bins.uop_remaining: 0 -> 2`,
				`ledger: {"actor":"ALN_001","after_uop":0,"before_uop":0,"detail":null,"loader_id":null,"metadata":{"old_payload":"PWOLD","sequence_id":1},"node_id":null,"op":"payload_bound_first_delta","order_id":null,"payload_code":"PWDECL","reason":"","source":"service/inventory_delta_service.go:firstDeltaBind","station":""}`,
				`ledger: {"actor":"ALN_001","after_uop":2,"before_uop":0,"detail":null,"loader_id":null,"metadata":{"bin_epoch":1,"delta":2,"healed":0,"reason":"produce_tick","sequence_id":1,"window_end":"<now>","window_start":"<now>","wire_delta":2,"wire_epoch":0},"node_id":"<node>","op":"bin_uop_delta","order_id":null,"payload_code":"PWDECL","reason":"produce_tick","source":"service/inventory_delta_service.go","station":""}`,
			}},
		{"rebind with inventory aboard",
			func() int64 { return f.heldBin(t, "PW-T-REBIND", f.fitsID, "PWOLD", 480) },
			func(id int64) func() error { return tick(id, f.declared, 3) },
			[]string{
				`bins.anomaly_at: null -> "<now>"`,
				`bins.manifest: {"items":[{"part_number":"OLD-PN"}]} -> {"items":[{"part_number":"PWDECL-PN-Z"},{"part_number":"PWDECL-PN-A"}]}`,
				`bins.payload_code: "PWOLD" -> "PWDECL"`,
				`bins.uop_remaining: 480 -> 483`,
				`ledger: {"actor":"ALN_001","after_uop":480,"before_uop":480,"detail":null,"loader_id":null,"metadata":{"delta":3,"inventory_at_rebind":480,"old_payload":"PWOLD","sequence_id":1},"node_id":null,"op":"payload_rebound_with_inventory","order_id":null,"payload_code":"PWDECL","reason":"","source":"service/inventory_delta_service.go:reboundWithInventory","station":""}`,
				`ledger: {"actor":"ALN_001","after_uop":483,"before_uop":480,"detail":null,"loader_id":null,"metadata":{"bin_epoch":1,"delta":3,"healed":0,"reason":"produce_tick","sequence_id":1,"window_end":"<now>","window_start":"<now>","wire_delta":3,"wire_epoch":0},"node_id":"<node>","op":"bin_uop_delta","order_id":null,"payload_code":"PWDECL","reason":"produce_tick","source":"service/inventory_delta_service.go","station":""}`,
			}},
		{"rebind keeps the first anomaly stamp",
			func() int64 {
				id := f.heldBin(t, "PW-T-ANOM", f.fitsID, "PWOLD", 40)
				f.stamp(t, id, "anomaly_at")
				return id
			},
			func(id int64) func() error { return tick(id, f.declared, 1) },
			[]string{
				`bins.manifest: {"items":[{"part_number":"OLD-PN"}]} -> {"items":[{"part_number":"PWDECL-PN-Z"},{"part_number":"PWDECL-PN-A"}]}`,
				`bins.payload_code: "PWOLD" -> "PWDECL"`,
				`bins.uop_remaining: 40 -> 41`,
				`ledger: {"actor":"ALN_001","after_uop":40,"before_uop":40,"detail":null,"loader_id":null,"metadata":{"delta":1,"inventory_at_rebind":40,"old_payload":"PWOLD","sequence_id":1},"node_id":null,"op":"payload_rebound_with_inventory","order_id":null,"payload_code":"PWDECL","reason":"","source":"service/inventory_delta_service.go:reboundWithInventory","station":""}`,
				`ledger: {"actor":"ALN_001","after_uop":41,"before_uop":40,"detail":null,"loader_id":null,"metadata":{"bin_epoch":1,"delta":1,"healed":0,"reason":"produce_tick","sequence_id":1,"window_end":"<now>","window_start":"<now>","wire_delta":1,"wire_epoch":0},"node_id":"<node>","op":"bin_uop_delta","order_id":null,"payload_code":"PWDECL","reason":"produce_tick","source":"service/inventory_delta_service.go","station":""}`,
			}},
		{"first tick onto an undeclared carrier",
			func() int64 { return f.heldBin(t, "PW-T-UNDECL", f.foreign, "", 0) },
			func(id int64) func() error { return tick(id, f.declared, 4) },
			[]string{
				`bins.manifest: null -> {"items":[{"part_number":"PWDECL-PN-Z"},{"part_number":"PWDECL-PN-A"}]}`,
				`bins.payload_code: "" -> "PWDECL"`,
				`bins.undeclared_carrier_at: null -> "<now>"`,
				`bins.uop_remaining: 0 -> 4`,
				`ledger: {"actor":"ALN_001","after_uop":0,"before_uop":0,"detail":null,"loader_id":null,"metadata":{"old_payload":"","sequence_id":1},"node_id":null,"op":"payload_bound_first_delta","order_id":null,"payload_code":"PWDECL","reason":"","source":"service/inventory_delta_service.go:firstDeltaBind","station":""}`,
				`ledger: {"actor":"ALN_001","after_uop":4,"before_uop":0,"detail":null,"loader_id":null,"metadata":{"bin_epoch":1,"delta":4,"healed":0,"reason":"produce_tick","sequence_id":1,"window_end":"<now>","window_start":"<now>","wire_delta":4,"wire_epoch":0},"node_id":"<node>","op":"bin_uop_delta","order_id":null,"payload_code":"PWDECL","reason":"produce_tick","source":"service/inventory_delta_service.go","station":""}`,
			}},
		{"rebind keeps the first undeclared stamp",
			func() int64 {
				id := f.heldBin(t, "PW-T-UNDECL2", f.foreign, "PWOLD", 12)
				f.stamp(t, id, "undeclared_carrier_at")
				return id
			},
			func(id int64) func() error { return tick(id, f.declared, 1) },
			[]string{
				`bins.anomaly_at: null -> "<now>"`,
				`bins.manifest: {"items":[{"part_number":"OLD-PN"}]} -> {"items":[{"part_number":"PWDECL-PN-Z"},{"part_number":"PWDECL-PN-A"}]}`,
				`bins.payload_code: "PWOLD" -> "PWDECL"`,
				`bins.uop_remaining: 12 -> 13`,
				`ledger: {"actor":"ALN_001","after_uop":12,"before_uop":12,"detail":null,"loader_id":null,"metadata":{"delta":1,"inventory_at_rebind":12,"old_payload":"PWOLD","sequence_id":1},"node_id":null,"op":"payload_rebound_with_inventory","order_id":null,"payload_code":"PWDECL","reason":"","source":"service/inventory_delta_service.go:reboundWithInventory","station":""}`,
				`ledger: {"actor":"ALN_001","after_uop":13,"before_uop":12,"detail":null,"loader_id":null,"metadata":{"bin_epoch":1,"delta":1,"healed":0,"reason":"produce_tick","sequence_id":1,"window_end":"<now>","window_start":"<now>","wire_delta":1,"wire_epoch":0},"node_id":"<node>","op":"bin_uop_delta","order_id":null,"payload_code":"PWDECL","reason":"produce_tick","source":"service/inventory_delta_service.go","station":""}`,
			}},
		{"rebind clears a stale undeclared stamp",
			func() int64 {
				id := f.heldBin(t, "PW-T-CLEAR", f.foreign, "PWOLD", 12)
				f.stamp(t, id, "undeclared_carrier_at")
				return id
			},
			func(id int64) func() error { return tick(id, f.sparse, 1) },
			[]string{
				`bins.anomaly_at: null -> "<now>"`,
				`bins.manifest: {"items":[{"part_number":"OLD-PN"}]} -> {"items":[{"part_number":"PWSPARSE-PN-Z"},{"part_number":"PWSPARSE-PN-A"}]}`,
				`bins.payload_code: "PWOLD" -> "PWSPARSE"`,
				`bins.undeclared_carrier_at: "2026-02-03T04:05:06Z" -> null`,
				`bins.uop_remaining: 12 -> 13`,
				`ledger: {"actor":"ALN_001","after_uop":12,"before_uop":12,"detail":null,"loader_id":null,"metadata":{"delta":1,"inventory_at_rebind":12,"old_payload":"PWOLD","sequence_id":1},"node_id":null,"op":"payload_rebound_with_inventory","order_id":null,"payload_code":"PWSPARSE","reason":"","source":"service/inventory_delta_service.go:reboundWithInventory","station":""}`,
				`ledger: {"actor":"ALN_001","after_uop":13,"before_uop":12,"detail":null,"loader_id":null,"metadata":{"bin_epoch":1,"delta":1,"healed":0,"reason":"produce_tick","sequence_id":1,"window_end":"<now>","window_start":"<now>","wire_delta":1,"wire_epoch":0},"node_id":"<node>","op":"bin_uop_delta","order_id":null,"payload_code":"PWSPARSE","reason":"produce_tick","source":"service/inventory_delta_service.go","station":""}`,
			}},
	}
	for _, c := range cases {
		id := c.setup()
		assertEffects(t, c.name, f.effectsOf(t, id, c.write(id)), c.want)
	}
}

// TestProduceFinalize_EffectsOntoAHeldCarrier pins produce finalize, both
// entry points, on carriers a dispatched order holds.
func TestProduceFinalize_EffectsOntoAHeldCarrier(t *testing.T) {
	t.Parallel()
	f := setupEffectsFixture(t)
	seven := 7
	manifest := `{"items":[{"part_number":"PWDECL-PN-Z"}]}`

	cases := []struct {
		name  string
		setup func() int64
		write func(int64) func() error
		want  []string
	}{
		{"finalize with a manifest over a labelled carrier",
			func() int64 { return f.heldBin(t, "PW-F-MANI", f.fitsID, "PWOLD", 5) },
			func(id int64) func() error {
				return func() error { return f.mani.RecordProducedBin(id, manifest, f.declared, 40, fixedProducedAt, nil) }
			},
			[]string{
				`bins.delta_epoch: 1 -> 2`,
				`bins.manifest: {"items":[{"part_number":"OLD-PN"}]} -> {"items":[{"part_number":"PWDECL-PN-Z"}]}`,
				`bins.payload_code: "PWOLD" -> "PWDECL"`,
				`bins.uop_remaining: 5 -> 40`,
				`bins.updated_at: "<now>" -> "<now>"`,
				`ledger: {"actor":"","after_uop":40,"before_uop":5,"detail":null,"loader_id":null,"metadata":null,"node_id":"<node>","op":"set_for_production","order_id":null,"payload_code":"PWDECL","reason":"","source":"service/bin_manifest.go:SetForProduction","station":""}`,
				`ledger: {"actor":"","after_uop":40,"before_uop":40,"detail":{"loaded_at":"2026-01-02T03:04:05Z"},"loader_id":null,"metadata":null,"node_id":"<node>","op":"manifest_confirmed","order_id":null,"payload_code":"PWDECL","reason":"","source":"service/bin_manifest.go:Confirm","station":""}`,
				`outbox: topic=shingo.dispatch.effects-test type=data.inventory.uop_adjustment station=edge-pw.line env=data src={"role":"core","station":"core.test"} dst={"role":"edge","station":"edge-pw.line"} subject=inventory.uop_adjustment body={"actor":"core","adjusted_at":"<now>","bin_id":"<bin>","core_node_name":"PW-SEAT","epoch":2,"new_remaining":40}`,
			}},
		{"finalize with no produced_at takes server time",
			func() int64 { return f.heldBin(t, "PW-F-NOW", f.fitsID, "", 0) },
			func(id int64) func() error {
				return func() error { return f.mani.RecordProducedBin(id, manifest, f.declared, 40, "", nil) }
			},
			[]string{
				`bins.delta_epoch: 1 -> 2`,
				`bins.loaded_at: null -> "<now>"`,
				`bins.manifest: null -> {"items":[{"part_number":"PWDECL-PN-Z"}]}`,
				`bins.manifest_confirmed: false -> true`,
				`bins.payload_code: "" -> "PWDECL"`,
				`bins.uop_remaining: 0 -> 40`,
				`bins.updated_at: "<now>" -> "<now>"`,
				`ledger: {"actor":"","after_uop":40,"before_uop":0,"detail":null,"loader_id":null,"metadata":null,"node_id":"<node>","op":"set_for_production","order_id":null,"payload_code":"PWDECL","reason":"","source":"service/bin_manifest.go:SetForProduction","station":""}`,
				`ledger: {"actor":"","after_uop":40,"before_uop":40,"detail":{"loaded_at":"<now>"},"loader_id":null,"metadata":null,"node_id":"<node>","op":"manifest_confirmed","order_id":null,"payload_code":"PWDECL","reason":"","source":"service/bin_manifest.go:Confirm","station":""}`,
				`outbox: topic=shingo.dispatch.effects-test type=data.inventory.uop_adjustment station=edge-pw.line env=data src={"role":"core","station":"core.test"} dst={"role":"edge","station":"edge-pw.line"} subject=inventory.uop_adjustment body={"actor":"core","adjusted_at":"<now>","bin_id":"<bin>","core_node_name":"PW-SEAT","epoch":2,"new_remaining":40}`,
			}},
		{"finalize from the template at standard pack",
			func() int64 { return f.heldBin(t, "PW-F-TPL", f.fitsID, "", 0) },
			func(id int64) func() error {
				return func() error { return f.mani.RecordProducedBinFromTemplate(id, f.declared, nil, fixedProducedAt, nil) }
			},
			[]string{
				`bins.delta_epoch: 1 -> 2`,
				`bins.loaded_at: null -> "2026-01-02T03:04:05Z"`,
				`bins.manifest: null -> {"items":[{"part_number":"PWDECL-PN-Z"},{"part_number":"PWDECL-PN-A"}]}`,
				`bins.manifest_confirmed: false -> true`,
				`bins.payload_code: "" -> "PWDECL"`,
				`bins.uop_remaining: 0 -> 500`,
				`bins.updated_at: "<now>" -> "<now>"`,
				`ledger: {"actor":"","after_uop":500,"before_uop":0,"detail":null,"loader_id":null,"metadata":null,"node_id":"<node>","op":"set_for_production","order_id":null,"payload_code":"PWDECL","reason":"","source":"service/bin_manifest.go:SetForProduction","station":""}`,
				`ledger: {"actor":"","after_uop":500,"before_uop":500,"detail":{"loaded_at":"2026-01-02T03:04:05Z"},"loader_id":null,"metadata":null,"node_id":"<node>","op":"manifest_confirmed","order_id":null,"payload_code":"PWDECL","reason":"","source":"service/bin_manifest.go:Confirm","station":""}`,
				`outbox: topic=shingo.dispatch.effects-test type=data.inventory.uop_adjustment station=edge-pw.line env=data src={"role":"core","station":"core.test"} dst={"role":"edge","station":"edge-pw.line"} subject=inventory.uop_adjustment body={"actor":"core","adjusted_at":"<now>","bin_id":"<bin>","core_node_name":"PW-SEAT","epoch":2,"new_remaining":500}`,
			}},
		{"finalize from the template with a count",
			func() int64 { return f.heldBin(t, "PW-F-TPL7", f.fitsID, "PWOLD", 2) },
			func(id int64) func() error {
				return func() error {
					return f.mani.RecordProducedBinFromTemplate(id, f.declared, &seven, fixedProducedAt, nil)
				}
			},
			[]string{
				`bins.delta_epoch: 1 -> 2`,
				`bins.manifest: {"items":[{"part_number":"OLD-PN"}]} -> {"items":[{"part_number":"PWDECL-PN-Z"},{"part_number":"PWDECL-PN-A"}]}`,
				`bins.payload_code: "PWOLD" -> "PWDECL"`,
				`bins.uop_remaining: 2 -> 7`,
				`bins.updated_at: "<now>" -> "<now>"`,
				`ledger: {"actor":"","after_uop":7,"before_uop":2,"detail":null,"loader_id":null,"metadata":null,"node_id":"<node>","op":"set_for_production","order_id":null,"payload_code":"PWDECL","reason":"","source":"service/bin_manifest.go:SetForProduction","station":""}`,
				`ledger: {"actor":"","after_uop":7,"before_uop":7,"detail":{"loaded_at":"2026-01-02T03:04:05Z"},"loader_id":null,"metadata":null,"node_id":"<node>","op":"manifest_confirmed","order_id":null,"payload_code":"PWDECL","reason":"","source":"service/bin_manifest.go:Confirm","station":""}`,
				`outbox: topic=shingo.dispatch.effects-test type=data.inventory.uop_adjustment station=edge-pw.line env=data src={"role":"core","station":"core.test"} dst={"role":"edge","station":"edge-pw.line"} subject=inventory.uop_adjustment body={"actor":"core","adjusted_at":"<now>","bin_id":"<bin>","core_node_name":"PW-SEAT","epoch":2,"new_remaining":7}`,
			}},
		{"finalize onto an undeclared carrier keeps the first stamp",
			func() int64 {
				id := f.heldBin(t, "PW-F-UNDECL", f.foreign, "PWOLD", 3)
				f.stamp(t, id, "undeclared_carrier_at")
				return id
			},
			func(id int64) func() error {
				return func() error { return f.mani.RecordProducedBin(id, manifest, f.declared, 9, fixedProducedAt, nil) }
			},
			[]string{
				`bins.delta_epoch: 1 -> 2`,
				`bins.manifest: {"items":[{"part_number":"OLD-PN"}]} -> {"items":[{"part_number":"PWDECL-PN-Z"}]}`,
				`bins.payload_code: "PWOLD" -> "PWDECL"`,
				`bins.uop_remaining: 3 -> 9`,
				`bins.updated_at: "<now>" -> "<now>"`,
				`ledger: {"actor":"","after_uop":9,"before_uop":3,"detail":null,"loader_id":null,"metadata":null,"node_id":"<node>","op":"set_for_production","order_id":null,"payload_code":"PWDECL","reason":"","source":"service/bin_manifest.go:SetForProduction","station":""}`,
				`ledger: {"actor":"","after_uop":9,"before_uop":9,"detail":{"loaded_at":"2026-01-02T03:04:05Z"},"loader_id":null,"metadata":null,"node_id":"<node>","op":"manifest_confirmed","order_id":null,"payload_code":"PWDECL","reason":"","source":"service/bin_manifest.go:Confirm","station":""}`,
				`outbox: topic=shingo.dispatch.effects-test type=data.inventory.uop_adjustment station=edge-pw.line env=data src={"role":"core","station":"core.test"} dst={"role":"edge","station":"edge-pw.line"} subject=inventory.uop_adjustment body={"actor":"core","adjusted_at":"<now>","bin_id":"<bin>","core_node_name":"PW-SEAT","epoch":2,"new_remaining":9}`,
			}},
		{"finalize onto an undeclared carrier stamps it",
			func() int64 { return f.heldBin(t, "PW-F-UNDECL2", f.foreign, "", 0) },
			func(id int64) func() error {
				return func() error { return f.mani.RecordProducedBin(id, manifest, f.declared, 9, fixedProducedAt, nil) }
			},
			[]string{
				`bins.delta_epoch: 1 -> 2`,
				`bins.loaded_at: null -> "2026-01-02T03:04:05Z"`,
				`bins.manifest: null -> {"items":[{"part_number":"PWDECL-PN-Z"}]}`,
				`bins.manifest_confirmed: false -> true`,
				`bins.payload_code: "" -> "PWDECL"`,
				`bins.undeclared_carrier_at: null -> "<now>"`,
				`bins.uop_remaining: 0 -> 9`,
				`bins.updated_at: "<now>" -> "<now>"`,
				`ledger: {"actor":"","after_uop":9,"before_uop":0,"detail":null,"loader_id":null,"metadata":null,"node_id":"<node>","op":"set_for_production","order_id":null,"payload_code":"PWDECL","reason":"","source":"service/bin_manifest.go:SetForProduction","station":""}`,
				`ledger: {"actor":"","after_uop":9,"before_uop":9,"detail":{"loaded_at":"2026-01-02T03:04:05Z"},"loader_id":null,"metadata":null,"node_id":"<node>","op":"manifest_confirmed","order_id":null,"payload_code":"PWDECL","reason":"","source":"service/bin_manifest.go:Confirm","station":""}`,
				`outbox: topic=shingo.dispatch.effects-test type=data.inventory.uop_adjustment station=edge-pw.line env=data src={"role":"core","station":"core.test"} dst={"role":"edge","station":"edge-pw.line"} subject=inventory.uop_adjustment body={"actor":"core","adjusted_at":"<now>","bin_id":"<bin>","core_node_name":"PW-SEAT","epoch":2,"new_remaining":9}`,
			}},
		{"finalize clears a stale undeclared stamp",
			func() int64 {
				id := f.heldBin(t, "PW-F-CLEAR", f.foreign, "PWOLD", 3)
				f.stamp(t, id, "undeclared_carrier_at")
				return id
			},
			func(id int64) func() error {
				return func() error { return f.mani.RecordProducedBinFromTemplate(id, f.sparse, nil, fixedProducedAt, nil) }
			},
			[]string{
				`bins.delta_epoch: 1 -> 2`,
				`bins.manifest: {"items":[{"part_number":"OLD-PN"}]} -> {"items":[{"part_number":"PWSPARSE-PN-Z"},{"part_number":"PWSPARSE-PN-A"}]}`,
				`bins.payload_code: "PWOLD" -> "PWSPARSE"`,
				`bins.undeclared_carrier_at: "2026-02-03T04:05:06Z" -> null`,
				`bins.uop_remaining: 3 -> 500`,
				`bins.updated_at: "<now>" -> "<now>"`,
				`ledger: {"actor":"","after_uop":500,"before_uop":3,"detail":null,"loader_id":null,"metadata":null,"node_id":"<node>","op":"set_for_production","order_id":null,"payload_code":"PWSPARSE","reason":"","source":"service/bin_manifest.go:SetForProduction","station":""}`,
				`ledger: {"actor":"","after_uop":500,"before_uop":500,"detail":{"loaded_at":"2026-01-02T03:04:05Z"},"loader_id":null,"metadata":null,"node_id":"<node>","op":"manifest_confirmed","order_id":null,"payload_code":"PWSPARSE","reason":"","source":"service/bin_manifest.go:Confirm","station":""}`,
				`outbox: topic=shingo.dispatch.effects-test type=data.inventory.uop_adjustment station=edge-pw.line env=data src={"role":"core","station":"core.test"} dst={"role":"edge","station":"edge-pw.line"} subject=inventory.uop_adjustment body={"actor":"core","adjusted_at":"<now>","bin_id":"<bin>","core_node_name":"PW-SEAT","epoch":2,"new_remaining":500}`,
			}},
		{"finalize leaves an anomaly stamp alone",
			func() int64 {
				id := f.heldBin(t, "PW-F-ANOM", f.fitsID, "PWOLD", 3)
				f.stamp(t, id, "anomaly_at")
				return id
			},
			func(id int64) func() error {
				return func() error { return f.mani.RecordProducedBin(id, manifest, f.declared, 11, fixedProducedAt, nil) }
			},
			[]string{
				`bins.delta_epoch: 1 -> 2`,
				`bins.manifest: {"items":[{"part_number":"OLD-PN"}]} -> {"items":[{"part_number":"PWDECL-PN-Z"}]}`,
				`bins.payload_code: "PWOLD" -> "PWDECL"`,
				`bins.uop_remaining: 3 -> 11`,
				`bins.updated_at: "<now>" -> "<now>"`,
				`ledger: {"actor":"","after_uop":11,"before_uop":3,"detail":null,"loader_id":null,"metadata":null,"node_id":"<node>","op":"set_for_production","order_id":null,"payload_code":"PWDECL","reason":"","source":"service/bin_manifest.go:SetForProduction","station":""}`,
				`ledger: {"actor":"","after_uop":11,"before_uop":11,"detail":{"loaded_at":"2026-01-02T03:04:05Z"},"loader_id":null,"metadata":null,"node_id":"<node>","op":"manifest_confirmed","order_id":null,"payload_code":"PWDECL","reason":"","source":"service/bin_manifest.go:Confirm","station":""}`,
				`outbox: topic=shingo.dispatch.effects-test type=data.inventory.uop_adjustment station=edge-pw.line env=data src={"role":"core","station":"core.test"} dst={"role":"edge","station":"edge-pw.line"} subject=inventory.uop_adjustment body={"actor":"core","adjusted_at":"<now>","bin_id":"<bin>","core_node_name":"PW-SEAT","epoch":2,"new_remaining":11}`,
			}},
	}
	for _, c := range cases {
		id := c.setup()
		assertEffects(t, c.name, f.effectsOf(t, id, c.write(id)), c.want)
	}
}
