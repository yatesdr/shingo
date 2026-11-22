package messaging

// The feed compare replayed over four saved sim-rig runs
// (testdata/feed_replay.txt): each station's heartbeat quotes, what Core
// answered, and the copies it sent between, in wire order, through the real
// feedState. The fixture's header has the format.

import (
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
)

type replayEvent struct {
	kind           string // h compare, p push, d sourcing delta
	at             time.Time
	quote, current string
	sent           []string
}

type replaySeries struct {
	run, station, key string
	events            []replayEvent
	beats             []int // index in events of each compare
}

func readFeedReplay(t *testing.T) []*replaySeries {
	t.Helper()
	b, err := os.ReadFile("testdata/feed_replay.txt")
	if err != nil {
		t.Fatalf("read the replay fixture: %v", err)
	}
	// An id stands for one digest within its series; 0 is "".
	digest := func(id string) string {
		if id == "0" {
			return ""
		}
		return "d" + id
	}
	base := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	var out []*replaySeries
	var run string
	var s *replaySeries
	for n, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		switch {
		case f[0] == "run" && len(f) == 2:
			run = f[1]
		case f[0] == "feed" && len(f) == 3:
			s = &replaySeries{run: run, station: "edge" + f[1], key: f[2]}
			out = append(out, s)
		case f[0] == "h" && len(f) == 4:
			ms, err := strconv.ParseInt(f[1], 10, 64)
			if err != nil {
				t.Fatalf("fixture line %d: %v", n+1, err)
			}
			s.beats = append(s.beats, len(s.events))
			s.events = append(s.events, replayEvent{kind: f[0], at: base.Add(time.Duration(ms) * time.Millisecond),
				quote: digest(f[2]), current: digest(f[3])})
		case (f[0] == "p" || f[0] == "d") && len(f) > 1:
			ev := replayEvent{kind: f[0], at: base}
			if len(s.beats) > 0 {
				ev.at = s.events[s.beats[len(s.beats)-1]].at.Add(time.Millisecond)
			}
			for _, id := range f[1:] {
				ev.sent = append(ev.sent, digest(id))
			}
			s.events = append(s.events, ev)
		default:
			t.Fatalf("fixture line %d: unknown %q", n+1, line)
		}
	}
	if len(out) == 0 {
		t.Fatal("the replay fixture holds no series")
	}
	return out
}

// play runs the series through a fresh feedState and returns the heartbeat
// each flag went up at. deltas: the sourcing monitor's sends are recorded.
// From heartbeat frozen on (-1 for never) the Edge quotes what it quoted there.
func (s *replaySeries) play(deltas bool, frozen int) (raised []int) {
	f := newFeedState()
	beat, stuck := -1, ""
	for _, ev := range s.events {
		switch ev.kind {
		case "p", "d":
			if ev.kind == "p" || deltas {
				for _, d := range ev.sent {
					f.recordPush(s.station, s.key, d, ev.at)
				}
			}
		case "h":
			beat++
			quote := ev.quote
			if frozen >= 0 && beat >= frozen {
				if beat == frozen {
					stuck = quote
				}
				quote = stuck
			}
			before := len(f.flagsFor(s.station))
			f.compare(s.station, s.key, ev.current, quote, ev.at)
			if len(f.flagsFor(s.station)) > before {
				raised = append(raised, beat)
			}
		}
	}
	return raised
}

// Four saved runs, every copy Core sent recorded: no flag.
func TestFeedReplay_TheSavedRunsFlagNothing(t *testing.T) {
	t.Parallel()
	flags, runs := map[string]int{}, map[string]bool{}
	var where []string
	for _, s := range readFeedReplay(t) {
		runs[s.run] = true
		for _, beat := range s.play(true, -1) {
			flags[s.run]++
			where = append(where, fmt.Sprintf("%s %s %s at heartbeat %d", s.run, s.station, s.key, beat+1))
		}
	}
	if len(runs) != 4 {
		t.Fatalf("replayed %d runs, want the four", len(runs))
	}
	if len(flags) != 0 {
		t.Errorf("flags by run %v, want none; every quote there was a copy Core sent:\n%s", flags, strings.Join(where, "\n"))
	}
}

// The instrument: the same runs with only the sends the v0.18.0 and v0.19.0
// Cores recorded (the sourcing monitor's left out). At v0.19.0 this is the
// count of flags those runs logged, 4, 5, 8, 6.
func TestFeedReplay_OnlyTheSendsTheSavedCoresRecorded(t *testing.T) {
	t.Parallel()
	got := map[string]int{}
	for _, s := range readFeedReplay(t) {
		got[s.run] += len(s.play(false, -1))
	}
	want := map[string]int{
		"p4-tip-8aa5bf8c":          4,
		"p4-tip-44bf04a3":          5,
		"p4-base-v0.18.0-8aa5bf8c": 8,
		"p4-base-v0.18.0-44bf04a3": 6,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("flags by run = %v, want %v", got, want)
	}
}

// An Edge that stops applying at any heartbeat of the saved runs is flagged by
// its third heartbeat stuck, however deep its quote was in what was sent. The
// positions left out are those where Core's value or a later send came back to
// the stuck digest, which is that Edge holding it, or where the guard held one
// of the three sends.
func TestFeedReplay_AFrozenQuoteInTheSavedRunsFlags(t *testing.T) {
	t.Parallel()
	tried := map[string]int{}
	for _, s := range readFeedReplay(t) {
		if s.key != protocol.FeedBins && s.key != protocol.FeedSourcing {
			continue
		}
	positions:
		for at := 0; at+2 < len(s.beats); at++ {
			first, third := s.beats[at], s.beats[at+2]
			stuck := s.events[first].quote
			if at > 0 && s.events[s.beats[at-1]].current == s.events[first].current {
				continue // the guard may hold the first of the three
			}
			currents := map[string]bool{}
			for _, ev := range s.events[first : third+1] {
				for _, d := range ev.sent {
					if d == stuck {
						continue positions
					}
				}
				if ev.kind == "h" {
					if ev.current == stuck {
						continue positions
					}
					currents[ev.current] = true
				}
			}
			if len(currents) != 3 {
				continue
			}
			tried[s.key]++
			flagged := false
			for _, beat := range s.play(true, at) {
				flagged = flagged || beat <= at+2
			}
			if !flagged {
				t.Errorf("%s %s %s stuck from heartbeat %d: no flag by the third", s.run, s.station, s.key, at+1)
			}
		}
	}
	if tried[protocol.FeedBins] < 100 || tried[protocol.FeedSourcing] < 12 {
		t.Fatalf("injected %v: the fixture no longer exercises the frozen quote", tried)
	}
	t.Logf("frozen quotes injected: %v", tried)
}
