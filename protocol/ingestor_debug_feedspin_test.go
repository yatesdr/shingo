package protocol

import (
	"strings"
	"sync"
	"testing"
)

// ingestor_debug_feedspin_test.go — which debug lines HandleRaw writes for a
// message addressed to this station and for one addressed to another.
//
// Today the header line is written before the station filter, so an Edge logs
// a header line for every message on the shared topic, most of them for other
// stations. The raw preview line is written before the filter too.
//
// after (C1): the header line moves after the station filter; the raw line
// stays where it is (C1 names only the header line).
func TestIngestor_HeaderDebugLineVsStationFilter_FeedsPin(t *testing.T) {
	const self = "edge.test"
	build := func(dst string) []byte {
		t.Helper()
		env, err := NewDataEnvelope(SubjectSupplyRefusalState,
			Address{Role: RoleCore},
			Address{Role: RoleEdge, Station: dst},
			&SupplyRefusalState{Action: SupplyRefusalOpened, LoaderNode: "LN-1", PayloadCode: "P-1"})
		if err != nil {
			t.Fatalf("build envelope: %v", err)
		}
		raw, err := env.Encode()
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return raw
	}

	cases := []struct {
		name       string
		dst        string
		wantPrefix []string // the debug line prefixes, in order
		dispatched bool
		after      string
		label      string
	}{
		{"this station", self, []string{"raw:", "header:"}, true, "same", "C1"},
		{"broadcast", StationBroadcast, []string{"raw:", "header:"}, true, "same", "C1"},
		{"another station", "edge.other", []string{"raw:"}, false, "raw: only (no header line)", "C1"}, // C1: was raw:,header:
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var lines []string
			ing := NewIngestor(func(hdr *RawHeader) bool {
				return hdr.Dst.Station == self || hdr.Dst.Station == StationBroadcast
			})
			ing.DebugLog = func(format string, _ ...any) {
				mu.Lock()
				defer mu.Unlock()
				lines = append(lines, strings.SplitN(format, " ", 2)[0])
			}
			dispatched := false
			ing.Dispatch = func(*Envelope) { dispatched = true }

			ing.HandleRaw(build(tc.dst))

			mu.Lock()
			got := strings.Join(lines, ",")
			mu.Unlock()
			if want := strings.Join(tc.wantPrefix, ","); got != want {
				t.Errorf("debug lines = %q, want %q (after: %s, %s)", got, want, tc.after, tc.label)
			}
			if dispatched != tc.dispatched {
				t.Errorf("dispatched = %v, want %v (after: same, %s)", dispatched, tc.dispatched, tc.label)
			}
		})
	}
}
