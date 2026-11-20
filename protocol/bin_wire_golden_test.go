package protocol

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The node-bins row and the CLEAR request, byte for byte as they serialise
// today. Fields added to either later are omitempty, so a plant that sets none
// of them must still send exactly these bytes; a byte that moves here is a wire
// change at every plant, not only the ones that use the new field.
//
// Each value is rendered twice and the two renderings compared before the
// golden, so a non-deterministic encoding fails on its own line rather than as
// a golden mismatch.

func renderTwice(t *testing.T, v any) string {
	t.Helper()
	first, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	second, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal again: %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("two renderings differ:\n  %s\n  %s", first, second)
	}
	return string(first)
}

func TestNodeBinInfo_Golden(t *testing.T) {
	t.Parallel()
	manifest := `{"items":[{"part_number":"PART-A","quantity":40}]}`
	for _, tc := range []struct {
		name string
		row  NodeBinInfo
		want string
	}{
		{
			name: "no bin at the node",
			row:  NodeBinInfo{NodeName: "SMN_A1"},
			want: `{"node_name":"SMN_A1","uop_remaining":0,"delta_epoch":0,"manifest_confirmed":false,"occupied":false}`,
		},
		{
			name: "a full",
			row: NodeBinInfo{
				NodeName: "SMN_A1", BinID: 27, BinLabel: "TOTE-1", BinTypeCode: "TOTE",
				PayloadCode: "PART-A", UOPRemaining: 40, DeltaEpoch: 3,
				Manifest: &manifest, ManifestConfirmed: true, Occupied: true,
			},
			want: `{"node_name":"SMN_A1","bin_id":27,"bin_label":"TOTE-1","bin_type_code":"TOTE",` +
				`"payload_code":"PART-A","uop_remaining":40,"delta_epoch":3,` +
				`"manifest":"{\"items\":[{\"part_number\":\"PART-A\",\"quantity\":40}]}",` +
				`"manifest_confirmed":true,"occupied":true}`,
		},
		{
			name: "a bare empty",
			row: NodeBinInfo{
				NodeName: "SMN_A1", BinID: 28, BinLabel: "TOTE-2", BinTypeCode: "TOTE-BARE",
				Bare: true, DeltaEpoch: 4, Occupied: true,
			},
			want: `{"node_name":"SMN_A1","bin_id":28,"bin_label":"TOTE-2","bin_type_code":"TOTE-BARE",` +
				`"bare":true,"uop_remaining":0,"delta_epoch":4,"manifest_confirmed":false,"occupied":true}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := renderTwice(t, tc.row); got != tc.want {
				t.Errorf("NodeBinInfo =\n  %s\nwant\n  %s", got, tc.want)
			}
		})
	}
}

func TestBinClearRequest_Golden(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		req  BinClearRequest
		want string
	}{
		{
			name: "the one bin, its own type",
			req:  BinClearRequest{NodeName: "SMN_A1"},
			want: `{"node_name":"SMN_A1"}`,
		},
		{
			name: "a named bin, re-typed",
			req:  BinClearRequest{NodeName: "SMN_A1", BinID: 27, BinTypeCode: "KD-1"},
			want: `{"node_name":"SMN_A1","bin_id":27,"bin_type_code":"KD-1"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := renderTwice(t, tc.req); got != tc.want {
				t.Errorf("BinClearRequest =\n  %s\nwant\n  %s", got, tc.want)
			}
		})
	}
}

var updateCountWireGolden = flag.Bool("update-count-wire-golden", false,
	"rewrite testdata/count-wire.golden from the count-bearing messages as they serialise now")

// countMessages are the wire's count-bearing messages (census D): each carries a
// bin's or a pile's count, a pack, a threshold or a ratio. Their JSON keys keep
// their names whatever the Go fields are called (a renamed key decodes as a
// silent zero on a peer of the other version), and at a ratio of 1 not a byte
// of any of them moves.
var countMessages = []any{
	OrderRequest{}, OrderDelivered{}, ComplexOrderRequest{}, UOPDisposition{}, OrderRelease{},
	ReleaseFence{}, OrderIngestRequest{}, IngestManifestItem{}, CatalogPayloadInfo{}, BinUOPDelta{},
	LinesideBucketLevel{}, UOPAdjustment{}, PlantClaim{}, ContainmentBin{}, NodeBinsRow{}, NodeBinInfo{},
	BinLoadItem{}, BinLoadRequest{}, BinLoadResponse{}, BinCountRequest{}, BinCountResponse{},
	BinClearResponse{}, HeldPartial{}, PartCreateRequest{}, HoldDropped{}, LinesideLevelEntry{},
	ManifestItem{},
}

// fillWire sets every exported field of v, recursively, to a fixed value of its
// kind: a number 803 (the owner's count above a pack of 800), a string its
// field's name, true, a fixed instant, one element in a slice or map. So every
// key a message can send is in its rendering, and the rendering is the same on
// every run. An interface field is left nil; depth stops a self-referencing type.
func fillWire(v reflect.Value, name string, depth int) {
	if depth > 4 || !v.CanSet() {
		return
	}
	if v.Type() == reflect.TypeOf(time.Time{}) {
		v.Set(reflect.ValueOf(time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)))
		return
	}
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(803)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(803)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(803.5)
	case reflect.String:
		v.SetString(name)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fillWire(p.Elem(), name, depth+1)
		v.Set(p)
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 { // []byte, json.RawMessage
			v.SetBytes([]byte(`{"k":803}`))
			return
		}
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fillWire(s.Index(0), name, depth+1)
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k, e := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		fillWire(k, "key", depth+1)
		fillWire(e, name, depth+1)
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if f := v.Type().Field(i); f.IsExported() {
				fillWire(v.Field(i), f.Name, depth+1)
			}
		}
	}
}

// TestCountMessages_Golden: every count-bearing message, byte for byte, empty
// and with every field set, against testdata/count-wire.golden. A line that
// moves is a wire change at every plant. Regenerate only for a named change:
//
//	go test ./protocol -run TestCountMessages_Golden -update-count-wire-golden
func TestCountMessages_Golden(t *testing.T) {
	var b strings.Builder
	for _, m := range countMessages {
		typ := reflect.TypeOf(m)
		zero := reflect.New(typ).Elem()
		full := reflect.New(typ).Elem()
		fillWire(full, typ.Name(), 0)
		b.WriteString("## " + typ.Name() + "\n")
		b.WriteString("empty: " + renderTwice(t, zero.Interface()) + "\n")
		b.WriteString("full:  " + renderTwice(t, full.Interface()) + "\n\n")
	}
	path := filepath.Join("testdata", "count-wire.golden")
	if *updateCountWireGolden {
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no %s (%v); regenerate with go test ./protocol -run TestCountMessages_Golden -update-count-wire-golden", path, err)
	}
	if got := b.String(); got != strings.ReplaceAll(string(want), "\r\n", "\n") {
		gl, wl := strings.Split(got, "\n"), strings.Split(string(want), "\n")
		for i := 0; i < len(gl) && i < len(wl); i++ {
			if gl[i] != wl[i] {
				t.Fatalf("%s moved at line %d: a count-bearing message changed on the wire\n  golden: %s\n  now:    %s\n"+
					"Regenerate only for a named wire change: go test ./protocol -run TestCountMessages_Golden -update-count-wire-golden",
					path, i+1, wl[i], gl[i])
			}
		}
		t.Fatalf("%s: %d lines now, %d in the golden; regenerate only for a named wire change", path, len(gl), len(wl))
	}
}
