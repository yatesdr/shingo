//go:build ignore

// gen.go wrote the goldens beside it (peer_floor_wire_test.go reads them): one
// body per type with every field set, encoded by the protocol package of
// v0.19.0. It runs inside that tag's protocol tree, never inside this one:
//
//	git archive v0.19.0 protocol | tar -x -C <dir>
//	cp gen.go <dir>/protocol/ && cd <dir>/protocol && GOWORK=off go run gen.go <out-dir>
//
// They are that release's bytes: never regenerate them from this tree.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"shingo/protocol"
)

func main() {
	out := os.Args[1]
	const station = "stn-0e0e0e0e0e0e0e0e"
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	t1 := time.Date(2026, 1, 2, 3, 5, 6, 0, time.UTC)
	pending, age := int64(3), int64(1500)
	name, watch := "SYN / EDGE", "d-watch"
	binID, asOfNet, asOfSeq := int64(31), int64(41), int64(12)
	uop, code := 40, "SYN-PART-A"

	bodies := map[string]any{
		"EdgeRegister": protocol.EdgeRegister{
			StationID: station, Hostname: "syn-edge", Instance: "inst-1", Version: "v0.19.0",
			Timezone: "America/Chicago", SchemaVersion: 59,
			Catalog: []protocol.CellCatalogEntry{{CellLabel: "SYN-CELL", Processes: []protocol.CellProcessBinding{
				{ProcessID: 1, StyleID: 2, PLCName: "SYN-PLC", TagName: "SYN-TAG"}}}},
		},
		"EdgeHeartbeat": protocol.EdgeHeartbeat{
			StationID: station, Timezone: "America/Chicago", TickPending: &pending, TickOldestUnsentAgeMS: &age,
			Feeds: map[string]string{"bins": "d-bins"}, Owned: map[string]string{"claims": "d-owned"},
		},
		"EdgeRegistered": protocol.EdgeRegistered{
			StationID: station, Message: "registered", EphemeralTopic: "syn.ephemeral",
		},
		"EdgeHeartbeatAck": protocol.EdgeHeartbeatAck{
			StationID: station, ServerTS: t0, Timezone: "America/Chicago", DisplayName: &name,
			Feeds:    map[string]string{"bins": "d-bins"},
			Owned:    map[string]protocol.OwnedAnswer{"claims": {Digest: "d-owned", Members: map[string]string{"SYN-PROC": "d-claims"}}},
			Claims:   map[string]string{"SYN-PROC": "d-claims"},
			BinWatch: &watch, Asks: []string{"bin.count"}, EphemeralTopic: "syn.ephemeral",
		},
		"RobotWatch": protocol.RobotWatch{
			StationID: station, OrderUUIDs: []string{"00000000-0000-4000-8000-000000000001"},
		},
		"LinesideLevelReport": protocol.LinesideLevelReport{
			Station: station, ReportedAt: t0,
			Entries: []protocol.LinesideLevelEntry{{CoreNodeName: "SYN-LINE", PayloadCode: "SYN-PART-A",
				BinCount: 40, BinUOP: 5, BucketQty: 2, BinID: &binID, BinEpoch: 4, FlushedSeq: 12}},
		},
		"OrderDelivered": protocol.OrderDelivered{
			OrderUUID: "00000000-0000-4000-8000-000000000002", DeliveredAt: t0, StagedExpireAt: &t1,
			BinID: &binID, UOPRemaining: &uop, DeltaEpoch: 4, BinPayloadCode: &code,
			DeliveryNode: "SYN-DST", BinDestNode: "SYN-LINE", LandingSent: true,
		},
		"UOPAdjustment": protocol.UOPAdjustment{
			BinID: binID, CoreNodeName: "SYN-LINE", NewRemaining: 40, Actor: "syn-person", AdjustedAt: t0,
			Released: true, PlacedSeq: 9, Bound: true, Epoch: 4, AsOfNet: &asOfNet, AsOfSeq: &asOfSeq,
			AsOfStation: station, PayloadCode: "SYN-PART-A", PayloadKnown: true,
		},
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		panic(err)
	}
	for name, body := range bodies {
		b, err := json.Marshal(body)
		if err != nil {
			panic(err)
		}
		if err := os.WriteFile(filepath.Join(out, name+".json"), append(b, '\n'), 0o644); err != nil {
			panic(err)
		}
		fmt.Println(name, len(b))
	}
}
