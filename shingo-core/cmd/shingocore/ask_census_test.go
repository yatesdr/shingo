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
// (protocol/ask.go), the TestCensus_OrderCreationDoors precedent: the ask
// pair must not regrow the HTTP surface one undocumented kind at a time.
//
// The server is built as main builds it (buildAskServer); it answers exactly
// protocol.AskKinds. Each kind is answered by the www.Doors body of the HTTP
// door named beside it, and that door is routed in www/router.go to the
// handler named (the reseat, which has no HTTP door, by its own body), so the ask and the HTTP call answer through one body
// (www/door_ask_agreement_test.go holds the answers equal arm by arm). Only the home
// clear replays a repeat.
//
// A nil *www.Doors is enough: registration takes method values and nothing is
// invoked.
func TestCensus_AskKinds(t *testing.T) {
	t.Parallel()
	var d *www.Doors
	census := []struct {
		kind    string
		http    string
		handler string
		door    asks.Door
		replay  bool
	}{
		{protocol.AskBinLoad, "POST /api/telemetry/bin-load", "apiBinLoad", d.AskBinLoad, false},
		{protocol.AskBinCount, "POST /api/telemetry/bin-count", "apiBinCount", d.AskBinCount, false},
		{protocol.AskBinClear, "POST /api/telemetry/bin-clear", "apiBinClear", d.AskBinClear, false},
		{protocol.AskQualityHold, "POST /api/telemetry/bin-quality-hold", "apiBinQualityHold", d.AskQualityHold, false},
		// Authors orders and refuses a second clear of a live one: a repeat
		// gets its first answer.
		{protocol.AskClearHome, "POST /api/telemetry/clear-home", "apiClearHome", d.AskClearHome, true},
		{protocol.AskPreflight, "POST /api/inventory/preflight", "apiInventoryPreflight", d.AskPreflight, false},
		{protocol.AskReleasePoints, "POST /api/release/points", "apiReleasePoints", d.AskReleasePoints, false},
		{protocol.AskHeldPartials, "GET /api/telemetry/held-partials", "apiTelemetryHeldPartials", d.AskHeldPartials, false},
		// No HTTP door: answered only as an ask, by the landing verb.
		{protocol.AskReseat, "", "", d.AskReseat, false},
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

	registered := askDoors(d)
	if len(registered) != len(census) || len(census) != len(protocol.AskKinds()) {
		t.Fatalf("askDoors has %d kinds, the census %d, protocol.AskKinds %d",
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
		if r.kind != c.kind || r.http != c.http || r.replay != c.replay {
			t.Errorf("askDoors row %d = {%s %q replay:%v}, census {%s %q replay:%v}",
				i, r.kind, r.http, r.replay, c.kind, c.http, c.replay)
		}
		if reflect.ValueOf(r.door).Pointer() != reflect.ValueOf(c.door).Pointer() {
			t.Errorf("%s is registered over another body than the one its HTTP door %s answers through", c.kind, c.http)
		}
		if c.http == "" {
			continue // no HTTP door to route
		}
		method, path, _ := strings.Cut(c.http, " ")
		verb := strings.ToUpper(method[:1]) + strings.ToLower(method[1:])
		route := "r." + verb + `("` + strings.TrimPrefix(path, "/api") + `", h.` + c.handler + ")"
		if !strings.Contains(string(router), route) {
			t.Errorf("%s names %s, but www/router.go routes no %s", c.kind, c.http, route)
		}
	}
}
