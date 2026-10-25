package main

import (
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"shingo/protocol"
	"shingocore/asks"
	"shingocore/www"
)

// TestCensus_AskKinds fences the kinds Core answers on the ask pair
// (protocol/ask.go), the order census's precedent (TestCensus_Issuers): the ask pair
// must not regrow a surface one undocumented kind at a time.
//
// The server is built as main builds it (buildAskServer, over
// www.Doors.AskDoors); it answers exactly protocol.AskKinds, each kind over the
// www.Doors body named beside it. Only the home clear replays a repeat. No
// machine door is routed over HTTP any more: each retired route is absent from
// www/router.go, so Core's unauthenticated machine-write surface stays gone.
//
// A nil *www.Doors is enough: registration takes method values and nothing is
// invoked.
func TestCensus_AskKinds(t *testing.T) {
	t.Parallel()
	var d *www.Doors
	census := []struct {
		kind    string
		retired string // the HTTP route that answered through the same body, gone
		door    asks.Door
		replay  bool
	}{
		{protocol.AskBinLoad, `r.Post("/telemetry/bin-load"`, d.AskBinLoad, false},
		{protocol.AskBinCount, `r.Post("/telemetry/bin-count"`, d.AskBinCount, false},
		{protocol.AskBinClear, `r.Post("/telemetry/bin-clear"`, d.AskBinClear, false},
		{protocol.AskQualityHold, `r.Post("/telemetry/bin-quality-hold"`, d.AskQualityHold, false},
		// Authors orders and refuses a second clear of a live one: a repeat
		// gets its first answer.
		{protocol.AskClearHome, `r.Post("/telemetry/clear-home"`, d.AskClearHome, true},
		{protocol.AskPreflight, `r.Post("/inventory/preflight"`, d.AskPreflight, false},
		{protocol.AskReleasePoints, `r.Post("/release/points"`, d.AskReleasePoints, false},
		{protocol.AskHeldPartials, `r.Get("/telemetry/held-partials"`, d.AskHeldPartials, false},
		// Never had an HTTP door: answered by the landing verb.
		{protocol.AskReseat, "", d.AskReseat, false},
		// Born on the ask.
		{protocol.AskLoaderRetrieve, "", d.AskLoaderRetrieve, false},
	}

	s, err := buildAskServer(nil, nil, d)
	if err != nil {
		t.Fatalf("buildAskServer: %v", err)
	}
	want := append([]string(nil), protocol.AskKinds()...)
	sort.Strings(want)
	if got := s.Kinds(); !slices.Equal(got, want) {
		t.Errorf("the server answers %v, protocol.AskKinds is %v", got, want)
	}

	registered := d.AskDoors()
	if len(registered) != len(census) || len(census) != len(protocol.AskKinds()) {
		t.Fatalf("AskDoors has %d kinds, the census %d, protocol.AskKinds %d",
			len(registered), len(census), len(protocol.AskKinds()))
	}
	router, err := os.ReadFile("../../www/router.go")
	if err != nil {
		t.Fatalf("read www/router.go: %v", err)
	}
	for i, c := range census {
		if c.kind != protocol.AskKinds()[i] {
			t.Errorf("census row %d is %q, protocol.AskKinds has %q there", i, c.kind, protocol.AskKinds()[i])
		}
		r := registered[i]
		if r.Kind != c.kind || r.Replay != c.replay {
			t.Errorf("AskDoors row %d = {%s replay:%v}, census {%s replay:%v}", i, r.Kind, r.Replay, c.kind, c.replay)
		}
		if reflect.ValueOf(r.Door).Pointer() != reflect.ValueOf(c.door).Pointer() {
			t.Errorf("%s is registered over another body than the census names", c.kind)
		}
		if c.retired != "" && strings.Contains(string(router), c.retired) {
			t.Errorf("%s: www/router.go routes %s again — the machine doors are asks only", c.kind, c.retired)
		}
	}
}
