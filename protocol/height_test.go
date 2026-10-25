package protocol

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestHeight_Admits is the want's own table: every form against every carrier
// state. Tops & bottoms answers Layers(0) and TopsAndBottomsOnly alone; every
// other state answers every want but TopsAndBottomsOnly.
func TestHeight_Admits(t *testing.T) {
	t.Parallel()
	states := []CarrierState{CarrierLoaded, CarrierBare, CarrierTopsAndBottoms, CarrierLeftover,
		CarrierUnverified, CarrierEmpty}
	for _, k := range []struct {
		name string
		h    Height
		tb   bool // admits tops & bottoms
		rest bool // admits every other state
	}{
		{"none", Height{}, false, true},
		{"a stack", Layers(3), false, true},
		{"layers 0", Layers(0), true, true},
		{"tops & bottoms only", TopsAndBottomsOnly, true, false},
	} {
		for _, s := range states {
			want := k.rest
			if s == CarrierTopsAndBottoms {
				want = k.tb
			}
			if got := k.h.Admits(s); got != want {
				t.Errorf("%s admits %s = %v, want %v", k.name, s, got, want)
			}
		}
	}
}

// TestHeight_Stated: only a Layers want names layers; none and tops & bottoms
// only name none, and a stated 0 is a stated 0.
func TestHeight_Stated(t *testing.T) {
	t.Parallel()
	for _, k := range []struct {
		name   string
		h      Height
		layers int
		ok     bool
	}{
		{"none", Height{}, 0, false},
		{"layers 3", Layers(3), 3, true},
		{"layers 0", Layers(0), 0, true},
		{"tops & bottoms only", TopsAndBottomsOnly, 0, false},
	} {
		if n, ok := k.h.Stated(); n != k.layers || ok != k.ok {
			t.Errorf("%s: Stated() = %d, %v, want %d, %v", k.name, n, ok, k.layers, k.ok)
		}
	}
	if Layers(0) == TopsAndBottomsOnly || Layers(0) == (Height{}) {
		t.Error("layers 0 is its own want: neither none nor tops & bottoms only")
	}
}

// TestHeight_Text: each form has one spelling, read back to the same value, and
// a spelling no form has is refused, never read as none.
func TestHeight_Text(t *testing.T) {
	t.Parallel()
	for _, k := range []struct {
		h    Height
		text string
	}{{Height{}, ""}, {Layers(0), "0"}, {Layers(5), "5"}, {TopsAndBottomsOnly, "tops_bottoms"}} {
		b, err := k.h.MarshalText()
		if err != nil || string(b) != k.text || k.h.String() != k.text {
			t.Errorf("%#v spells %q (%v) / %q, want %q", k.h, b, err, k.h.String(), k.text)
		}
		var back Height
		if err := back.UnmarshalText([]byte(k.text)); err != nil || back != k.h {
			t.Errorf("%q reads back as %#v (%v), want %#v", k.text, back, err, k.h)
		}
	}
	for _, bad := range []string{"stack", "tops & bottoms", "3.5", " 3"} {
		var h Height
		if err := h.UnmarshalText([]byte(bad)); err == nil {
			t.Errorf("%q read as %#v, want it refused", bad, h)
		}
	}
}

// TestOrderRequest_Want_MixedVersion: the want rides the request as one
// string, is omitted when none is stated (every other order's wire is
// unchanged), and an older Core that has never heard of it decodes the empty
// retrieve it always did, which is why Core goes first.
func TestOrderRequest_Want_MixedVersion(t *testing.T) {
	t.Parallel()
	buf, err := json.Marshal(OrderRequest{OrderUUID: "u-1", OrderType: OrderTypeRetrieve, RetrieveEmpty: true,
		DeliveryNode: "OUT", SourceNode: "SUPER", Want: TopsAndBottomsOnly})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(buf), `"want":"tops_bottoms"`) {
		t.Errorf("the wire is %s, want the want as one string", buf)
	}
	var got OrderRequest
	if err := json.Unmarshal(buf, &got); err != nil || got.Want != TopsAndBottomsOnly || !got.RetrieveEmpty {
		t.Errorf("round trip = %+v (%v), want the stated tops & bottoms", got, err)
	}
	var oldCore struct {
		OrderType     OrderType `json:"order_type"`
		RetrieveEmpty bool      `json:"retrieve_empty"`
	}
	if err := json.Unmarshal(buf, &oldCore); err != nil || oldCore.OrderType != OrderTypeRetrieve || !oldCore.RetrieveEmpty {
		t.Errorf("an older Core read %+v (%v), want the plain empty retrieve", oldCore, err)
	}
	plain, err := json.Marshal(OrderRequest{OrderUUID: "u-2", OrderType: OrderTypeRetrieve, DeliveryNode: "LINE"})
	if err != nil || strings.Contains(string(plain), "want") {
		t.Errorf("a request stating no want sent %s (%v), want the key absent", plain, err)
	}
	if err := json.Unmarshal([]byte(`{"order_uuid":"u-3","want":"stack"}`), &got); err == nil {
		t.Error("a want no form spells was decoded, want the request refused")
	}
}

// TestOrderRequest_NoStagingNodeOrLoadType: neither rides the request. Core read
// neither: the staging node is the Edge's own row's (its orders page shows
// it), and nothing anywhere read a load type.
func TestOrderRequest_NoStagingNodeOrLoadType(t *testing.T) {
	t.Parallel()
	rt := reflect.TypeOf(OrderRequest{})
	for i := range rt.NumField() {
		tag := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]
		if tag == "staging_node" || tag == "load_type" {
			t.Errorf("OrderRequest.%s rides the wire as %q", rt.Field(i).Name, tag)
		}
	}
}
