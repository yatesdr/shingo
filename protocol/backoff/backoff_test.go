package backoff

import (
	"testing"
	"time"
)

func TestNew(t *testing.T) {
	b := New(100*time.Millisecond, 5*time.Second)
	if b.Base() != 100*time.Millisecond {
		t.Errorf("Base() = %v, want 100ms", b.Base())
	}
	if b.Max() != 5*time.Second {
		t.Errorf("Max() = %v, want 5s", b.Max())
	}
}

func TestNext_Doubles(t *testing.T) {
	b := New(100*time.Millisecond, 10*time.Second)

	d1 := b.Next()
	if d1 < 80*time.Millisecond || d1 > 120*time.Millisecond {
		t.Errorf("first Next() = %v, want ~100ms (±20%%)", d1)
	}

	d2 := b.Next()
	if d2 < 160*time.Millisecond || d2 > 240*time.Millisecond {
		t.Errorf("second Next() = %v, want ~200ms (±20%%)", d2)
	}

	d3 := b.Next()
	if d3 < 320*time.Millisecond || d3 > 480*time.Millisecond {
		t.Errorf("third Next() = %v, want ~400ms (±20%%)", d3)
	}
}

func TestNext_CapsAtMax(t *testing.T) {
	b := New(3*time.Second, 5*time.Second)

	_ = b.Next() // ~3s, internal becomes 6s -> capped to 5s
	d := b.Next()
	// 5s ±20% = 4s..6s
	if d < 4*time.Second || d > 6*time.Second {
		t.Errorf("capped Next() = %v, want ~5s (±20%%)", d)
	}

	// Should stay capped
	d2 := b.Next()
	if d2 < 4*time.Second || d2 > 6*time.Second {
		t.Errorf("stays capped Next() = %v, want ~5s (±20%%)", d2)
	}
}

func TestReset(t *testing.T) {
	b := New(100*time.Millisecond, 5*time.Second)

	_ = b.Next() // ~100ms
	_ = b.Next() // ~200ms
	_ = b.Next() // ~400ms

	b.Reset()

	d := b.Next()
	if d < 80*time.Millisecond || d > 120*time.Millisecond {
		t.Errorf("after Reset(), Next() = %v, want ~100ms (±20%%)", d)
	}
}

func TestNext_JitterRange(t *testing.T) {
	b := New(1*time.Second, 10*time.Second)

	var minVal, maxVal time.Duration
	for i := 0; i < 1000; i++ {
		b.Reset()
		d := b.Next()
		if i == 0 || d < minVal {
			minVal = d
		}
		if i == 0 || d > maxVal {
			maxVal = d
		}
	}

	// 1s ±20% = 800ms to 1200ms
	floor := 790 * time.Millisecond
	ceil := 1210 * time.Millisecond
	if minVal < floor {
		t.Errorf("min jitter %v below expected floor %v", minVal, floor)
	}
	if maxVal > ceil {
		t.Errorf("max jitter %v above expected ceil %v", maxVal, ceil)
	}
}
