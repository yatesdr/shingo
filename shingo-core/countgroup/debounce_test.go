package countgroup

import "testing"

// feedAll feeds a sequence of samples into a debouncer and returns
// the emit log: [(state-after, changed)]. Useful for table-driven tests.
func feedAll(d *debouncer, samples []bool) []struct {
	s       state
	changed bool
} {
	out := make([]struct {
		s       state
		changed bool
	}, len(samples))
	for i, v := range samples {
		s, c := d.feed(v)
		out[i] = struct {
			s       state
			changed bool
		}{s, c}
	}
	return out
}

func TestDebounceColdStartHoldsUntilOnThresholdHit(t *testing.T) {
	// 2-of-3 on, 3-of-3 off. First occupied sample must NOT emit —
	// needs two consecutive.
	d := newDebouncer(2, 3)

	if _, changed := d.feed(true); changed {
		t.Fatalf("first occupied sample emitted; should be held by cold-start fill")
	}
	if !d.isUnknown() {
		t.Fatalf("expected state=unknown after 1 sample, got %v", d.current)
	}

	s, changed := d.feed(true)
	if !changed || s != stateOn {
		t.Fatalf("second consecutive occupied should emit on; got (%v, changed=%v)", s, changed)
	}
}

func TestDebounceAsymmetricThresholds(t *testing.T) {
	// 2-of-3 on, 3-of-3 off. Validate that on takes fewer samples
	// than off (devA's design intent).
	d := newDebouncer(2, 3)

	// 2 empty samples — still unknown (off threshold is 3).
	d.feed(false)
	if _, changed := d.feed(false); changed {
		t.Fatalf("2 empties emitted off; need 3")
	}
	// Third empty sample commits state=off.
	if s, changed := d.feed(false); !changed || s != stateOff {
		t.Fatalf("expected off after 3 empties, got (%v, changed=%v)", s, changed)
	}

	// 1 occupied — pending but not committed.
	if _, changed := d.feed(true); changed {
		t.Fatalf("1 occupied emitted on; on threshold is 2")
	}
	// 2 occupied — commits state=on.
	if s, changed := d.feed(true); !changed || s != stateOn {
		t.Fatalf("expected on after 2 occupieds, got (%v, changed=%v)", s, changed)
	}
}

func TestDebounceTransientFlicker(t *testing.T) {
	// A stable ON stream interrupted by a single empty should NOT
	// cause an off emit — the off threshold of 3 protects against it.
	d := newDebouncer(2, 3)
	d.feed(true)
	d.feed(true) // state=on

	if s, changed := d.feed(false); changed {
		t.Fatalf("1 empty during stable-on emitted; should be absorbed. got (%v, changed=%v)", s, changed)
	}
	// Return to occupied — still on, no emit.
	if _, changed := d.feed(true); changed {
		t.Fatalf("return to occupied after 1 empty emitted")
	}
}

func TestDebounceOffRequiresConsecutive(t *testing.T) {
	d := newDebouncer(2, 3)
	d.feed(true)
	d.feed(true) // state=on

	// 2 empties, then an occupied — should NOT emit off.
	d.feed(false)
	d.feed(false)
	if _, changed := d.feed(true); changed {
		t.Fatalf("after empty/empty/occupied, state should still be on (no emit)")
	}
	// Now 3 consecutive empties.
	d.feed(false)
	d.feed(false)
	if s, changed := d.feed(false); !changed || s != stateOff {
		t.Fatalf("expected off after 3 consecutive empties; got (%v, changed=%v)", s, changed)
	}
}

func TestDebounceForceOnOverridesState(t *testing.T) {
	d := newDebouncer(2, 3)

	// From unknown: forceOn flips to On.
	if !d.forceOn() {
		t.Fatalf("forceOn on unknown should return true")
	}
	if d.current != stateOn {
		t.Fatalf("current should be on, got %v", d.current)
	}
	// Calling forceOn again is a no-op.
	if d.forceOn() {
		t.Fatalf("forceOn on already-on should return false (idempotent)")
	}
}

func TestDebounceForceOnFromOff(t *testing.T) {
	d := newDebouncer(2, 3)
	d.feed(false)
	d.feed(false)
	d.feed(false) // state=off
	if !d.forceOn() {
		t.Fatalf("forceOn on off should return true")
	}
	if d.current != stateOn {
		t.Fatalf("expected on after forceOn, got %v", d.current)
	}
}

func TestDebounceNoSpuriousEmitOnRepeatedSameState(t *testing.T) {
	d := newDebouncer(2, 3)
	d.feed(true)
	d.feed(true) // state=on, changed=true
	// Many more occupied samples — no further emits.
	for i := 0; i < 10; i++ {
		if _, changed := d.feed(true); changed {
			t.Fatalf("iter %d emitted during stable on", i)
		}
	}
}
