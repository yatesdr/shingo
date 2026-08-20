package clock

import (
	"testing"
	"time"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestManualNow(t *testing.T) {
	m := NewManual(epoch)
	if !m.Now().Equal(epoch) {
		t.Fatalf("Now=%v want %v", m.Now(), epoch)
	}
	m.Advance(90 * time.Second)
	if got := m.Now(); !got.Equal(epoch.Add(90 * time.Second)) {
		t.Fatalf("after advance Now=%v want +90s", got)
	}
}

func TestManualAfter(t *testing.T) {
	m := NewManual(epoch)
	ch := m.After(5 * time.Second)
	m.Advance(3 * time.Second)
	select {
	case <-ch:
		t.Fatal("After(5s) fired after only 3s")
	default:
	}
	m.Advance(2 * time.Second) // total 5s
	select {
	case got := <-ch:
		if !got.Equal(epoch.Add(5 * time.Second)) {
			t.Errorf("After delivered %v, want epoch+5s", got)
		}
	default:
		t.Fatal("After(5s) did not fire at 5s")
	}
}

func TestManualAfterZero(t *testing.T) {
	m := NewManual(epoch)
	select {
	case <-m.After(0):
	default:
		t.Fatal("After(0) should fire immediately")
	}
}

func TestManualTicker(t *testing.T) {
	m := NewManual(epoch)
	tk := m.NewTicker(time.Second)
	defer tk.Stop()
	ticks := 0
	for i := 0; i < 3; i++ {
		m.Advance(time.Second)
		select {
		case <-tk.C():
			ticks++
		default:
			t.Fatalf("ticker did not fire on advance %d", i+1)
		}
	}
	if ticks != 3 {
		t.Errorf("ticks=%d want 3", ticks)
	}
}

func TestManualTickerStop(t *testing.T) {
	m := NewManual(epoch)
	tk := m.NewTicker(time.Second)
	tk.Stop()
	m.Advance(3 * time.Second)
	select {
	case <-tk.C():
		t.Fatal("stopped ticker fired")
	default:
	}
}

// Advancing past several ticker intervals at once should not block (coalesce).
func TestManualTickerCoalesce(t *testing.T) {
	m := NewManual(epoch)
	tk := m.NewTicker(time.Second)
	defer tk.Stop()
	m.Advance(5 * time.Second) // 5 intervals, buffer holds 1
	got := 0
	for {
		select {
		case <-tk.C():
			got++
		default:
			if got == 0 {
				t.Fatal("ticker never fired across 5 intervals")
			}
			return
		}
	}
}
