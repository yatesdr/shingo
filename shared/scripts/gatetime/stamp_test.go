package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStamps(t *testing.T) {
	now := time.Now()
	got := stamps(&fakeMachine{acState: 1}, nil, now.Add(-time.Minute), now, "")
	if got != "ac=on samples=1 standby=0 load=unmeasured" {
		t.Errorf("not under a run: %q", got)
	}
	got = stamps(&fakeMachine{acState: 0, evErr: errors.New("no log")}, nil, now.Add(-time.Minute), now, "")
	if got != "ac=off samples=1 standby=unread load=unmeasured" {
		t.Errorf("battery, unreadable log: %q", got)
	}
	d := t.TempDir()
	live := resultLine{"samples": "40", "ac": "on", "foreign_max30": "0.31", "verdict": "clean"}
	if err := os.WriteFile(filepath.Join(d, "live.tsv"), []byte(live.String()+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, plants := plantFrom(&fakeMachine{acState: 1}, "standby")
	got = stamps(m, plants, now.Add(-time.Minute), now, d)
	if got != "ac=on samples=40 standby=2 load=clean foreign30=0.31 planted=standby" {
		t.Errorf("under a run: %q", got)
	}
}
