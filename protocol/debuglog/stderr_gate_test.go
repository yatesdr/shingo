package debuglog

import (
	"bytes"
	"strings"
	"testing"
)

// The stderr mirror is what reaches journald under systemd. Until 2026-07-25
// it was unconditional; these pin the allow-list that replaced it, and — the
// part that matters — that the ring buffer stays complete regardless, so
// muting a subsystem never costs an incident its trace.

func newTestLogger(t *testing.T) (*Logger, *bytes.Buffer) {
	t.Helper()
	l, err := New(64, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var buf bytes.Buffer
	l.SetStderr(&buf)
	return l, &buf
}

func TestStderrGate_NoFilterMirrorsEverything(t *testing.T) {
	l, buf := newTestLogger(t)

	l.Log("countgroup", "poll")
	l.Log("dispatch", "plan")

	out := buf.String()
	if !strings.Contains(out, "[countgroup] poll") || !strings.Contains(out, "[dispatch] plan") {
		t.Fatalf("default (nil filter) should mirror every subsystem, got:\n%s", out)
	}
}

func TestStderrGate_AllowListDropsOthers(t *testing.T) {
	l, buf := newTestLogger(t)
	l.SetStderrSubsystems([]string{"dispatch", "engine"})

	l.Log("countgroup", "poll")
	l.Log("rds", "GET /robotsStatus")
	l.Log("dispatch", "plan")

	out := buf.String()
	if strings.Contains(out, "countgroup") || strings.Contains(out, "rds") {
		t.Fatalf("muted subsystems reached stderr:\n%s", out)
	}
	if !strings.Contains(out, "[dispatch] plan") {
		t.Fatalf("allowed subsystem missing from stderr:\n%s", out)
	}
}

// The whole point of gating rather than deleting the dbg() calls: the browser
// log UI reads the ring buffer, and it must still show what the journal no
// longer does.
func TestStderrGate_RingBufferKeepsMutedSubsystems(t *testing.T) {
	l, buf := newTestLogger(t)
	l.SetStderrSubsystems([]string{"dispatch"})

	l.Log("countgroup", "occupancy now [AMR-03]")

	if strings.Contains(buf.String(), "countgroup") {
		t.Fatalf("muted subsystem reached stderr: %s", buf.String())
	}
	entries := l.Entries("countgroup")
	if len(entries) != 1 || entries[0].Message != "occupancy now [AMR-03]" {
		t.Fatalf("ring buffer lost the muted entry, got %#v", entries)
	}
	if subs := l.Subsystems(); len(subs) != 1 || subs[0] != "countgroup" {
		t.Fatalf("muted subsystem missing from Subsystems(), got %v", subs)
	}
}

func TestStderrGate_EmptySliceMutesEverything(t *testing.T) {
	l, buf := newTestLogger(t)
	l.SetStderrSubsystems([]string{})

	l.Log("dispatch", "plan")

	if buf.Len() != 0 {
		t.Fatalf("empty allow-list should mute the mirror, got: %s", buf.String())
	}
	if len(l.Entries("")) != 1 {
		t.Fatal("ring buffer should still have the entry")
	}
}

func TestStderrGate_NilRestoresFullMirror(t *testing.T) {
	l, buf := newTestLogger(t)
	l.SetStderrSubsystems([]string{"dispatch"})
	l.SetStderrSubsystems(nil)

	l.Log("countgroup", "poll")

	if !strings.Contains(buf.String(), "countgroup") {
		t.Fatalf("nil should clear the restriction, got: %s", buf.String())
	}
}

func TestStderrMirrors_ReportsEffectiveGate(t *testing.T) {
	l, _ := newTestLogger(t)

	if !l.StderrMirrors("countgroup") {
		t.Fatal("nil filter: everything mirrors")
	}
	l.SetStderrSubsystems([]string{"dispatch"})
	if l.StderrMirrors("countgroup") {
		t.Fatal("countgroup is not on the allow-list")
	}
	if !l.StderrMirrors("dispatch") {
		t.Fatal("dispatch is on the allow-list")
	}
	l.SetStderr(nil)
	if l.StderrMirrors("dispatch") {
		t.Fatal("no stderr writer: nothing mirrors")
	}
}
