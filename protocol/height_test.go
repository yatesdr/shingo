package protocol

import "testing"

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
