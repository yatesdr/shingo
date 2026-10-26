package service

import (
	"testing"
	"time"

	"shingocore/store/audit"
)

// Pure (no Postgres): keepFIFODate over carrier histories, newest first. The
// times and counts are Hopkinsville's 4x2 supermarket carts, 2026-10-19 to 22.
func TestKeepFIFODate(t *testing.T) {
	t.Parallel()
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	n := func(v int) *int { return &v }
	node := func(v int64) *int64 { return &v }
	loaded := at("2026-10-19T19:01:01Z")
	confirm := func(part string) audit.CarrierRow {
		return audit.CarrierRow{Op: audit.OpManifestConfirmed, Payload: part, NodeID: node(111), At: loaded, LoadedAt: &loaded}
	}
	set := func(part string, before int, nodeID int64, when string) audit.CarrierRow {
		return audit.CarrierRow{Op: audit.OpSetForProduction, Payload: part, BeforeUOP: n(before), NodeID: node(nodeID), At: at(when)}
	}
	clear := func(before int, nodeID int64, when string) audit.CarrierRow {
		return audit.CarrierRow{Op: audit.OpClearForReuse, BeforeUOP: n(before), NodeID: node(nodeID), At: at(when)}
	}

	cases := []struct {
		name string
		rows []audit.CarrierRow
		keep bool
	}{
		{"case 2: cleared at 80 and set again 9 s later",
			[]audit.CarrierRow{set("PIA02", 0, 111, "2026-10-21T13:21:11Z"), clear(80, 111, "2026-10-21T13:21:02Z"), confirm("PIA02")}, true},
		{"case 2: set again 13 minutes later, inside the window",
			[]audit.CarrierRow{set("PIA02", 0, 111, "2026-10-22T15:35:59Z"), clear(50, 111, "2026-10-22T15:22:51Z"), confirm("PIA02")}, true},
		{"case 2: set again 21 minutes later",
			[]audit.CarrierRow{set("PIA02", 0, 111, "2026-10-22T15:43:52Z"), clear(50, 111, "2026-10-22T15:22:51Z"), confirm("PIA02")}, false},
		{"case 2: another part after the clear is new stock",
			[]audit.CarrierRow{set("3405-PIA02", 0, 111, "2026-10-21T13:21:11Z"), clear(80, 111, "2026-10-21T13:21:02Z"), confirm("PIA02")}, false},
		{"case 2: set again at another spot",
			[]audit.CarrierRow{set("PIA02", 0, 125, "2026-10-21T13:21:11Z"), clear(80, 111, "2026-10-21T13:21:02Z"), confirm("PIA02")}, false},
		{"case 2: cleared already empty, so a refill",
			[]audit.CarrierRow{set("PIA02", 0, 111, "2026-10-21T13:21:11Z"), clear(0, 111, "2026-10-21T13:21:02Z"), confirm("PIA02")}, false},
		{"case 2: lifted between the clear and the set",
			[]audit.CarrierRow{set("PIA02", 0, 111, "2026-10-21T13:21:11Z"),
				{Op: audit.OpLifted, At: at("2026-10-21T13:21:05Z")}, clear(80, 111, "2026-10-21T13:21:02Z")}, false},
		{"case 1: the same part set again on a carrier holding 40",
			[]audit.CarrierRow{set("PIA02", 40, 111, "2026-10-21T13:21:11Z"), confirm("PIA02")}, true},
		{"case 1: the same part set again on a carrier at 0 is a refill",
			[]audit.CarrierRow{set("PIA02", 0, 111, "2026-10-21T13:21:11Z"), confirm("PIA02")}, false},
		{"case 1: a carrier restamped to another part is new stock",
			[]audit.CarrierRow{set("3B346-PIA01", -96, 106, "2026-10-26T12:20:31Z"), confirm("3405-PIA01")}, false},
		{"case 3: released at 0 and set again looks like a refill",
			[]audit.CarrierRow{set("PIA02", 0, 125, "2026-10-22T15:35:59Z"), {Op: audit.OpReleasedCaptureEmpty, At: at("2026-10-20T13:07:51Z")}}, false},
		{"a confirm with no set in front of it stamps now",
			[]audit.CarrierRow{confirm("PIA02"), set("PIA02", 40, 111, "2026-10-21T13:21:11Z")}, false},
	}
	for _, c := range cases {
		got, why := keepFIFODate(c.rows)
		switch {
		case c.keep && (got == nil || !got.Equal(loaded)):
			t.Errorf("%s: kept %v, want the carrier's date %v", c.name, got, loaded)
		case c.keep && why == "":
			t.Errorf("%s: kept the date with no reason for the record", c.name)
		case !c.keep && got != nil:
			t.Errorf("%s: kept %v (%s), want a new date", c.name, got, why)
		}
	}

	// A confirm written before the detail carried its date stamped now: its
	// own time is the date.
	old := confirm("PIA02")
	old.LoadedAt = nil
	got, _ := keepFIFODate([]audit.CarrierRow{set("PIA02", 40, 111, "2026-10-21T13:21:11Z"), old})
	if got == nil || !got.Equal(old.At) {
		t.Errorf("a confirm row with no date in its detail: kept %v, want its own time %v", got, old.At)
	}
}
