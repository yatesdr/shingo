package protocol

import (
	"strings"
	"sync"
	"testing"
)

// ingestor_debug_feedspin_test.go — which debug lines Ingestor.HandleRaw
// writes for a message addressed to this station, to every station, and to
// another station.
//
// The raw preview line is written for every message, before the signature
// check and the station filter. The header line is written only for a message
// the filter keeps, so an Edge on the shared topic does not log a header line
// for each message it drops. A message for another station gets the raw line
// alone and is not dispatched.
//
// It forbids moving the header line back above the filter. Until "perf(edge):
// sourcing writes only what changed, status shows the Core link, one stream
// per page" it sat there, and every Edge logged a header line for every other
// station's traffic.
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
	}{
		{"this station", self, []string{"raw:", "header:"}, true},
		{"broadcast", StationBroadcast, []string{"raw:", "header:"}, true},
		{"another station", "edge.other", []string{"raw:"}, false}, // no header line
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
				t.Errorf("debug lines = %q, want %q", got, want)
			}
			if dispatched != tc.dispatched {
				t.Errorf("dispatched = %v, want %v", dispatched, tc.dispatched)
			}
		})
	}
}
