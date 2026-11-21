//go:build ignore

// gen.go wrote the goldens beside it (peer_wire_test.go reads them): one
// message body per type, every field set, encoded by the protocol package it
// runs inside. It runs inside a deployed tag's protocol tree, never inside the
// current one, whose structs are what the goldens are checked against:
//
//	git archive <tag> protocol | tar -x -C <dir>
//	cp gen.go <dir>/protocol/ && cd <dir>/protocol && GOWORK=off go run gen.go <out-dir>
//
// Run at v0.17.2 and at v0.18.0 on 2026-11-21; the two runs wrote the same
// bytes, and those are the files here.
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
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	t1 := time.Date(2026, 1, 2, 3, 5, 6, 0, time.UTC)
	t2 := time.Date(2026, 1, 2, 3, 6, 7, 0, time.UTC)
	net := int64(41)
	oldParent, newParent := int64(7), int64(8)
	binID := int64(31)
	ack := t2

	bodies := map[string]any{
		"BinUOPDelta": protocol.BinUOPDelta{
			Station: "stn-0e0e0e0e0e0e0e0e", BinID: 31, PayloadCode: "SYN-PART-A", Delta: -3,
			Reason: "consume_tick", SequenceID: 12, Epoch: 4, WindowStart: t0, WindowEnd: t1, Net: &net,
		},
		"EdgeRegistered": protocol.EdgeRegistered{
			StationID: "stn-0e0e0e0e0e0e0e0e", Message: "registered", EphemeralTopic: "syn.ephemeral",
		},
		"EdgeHeartbeatAck": protocol.EdgeHeartbeatAck{
			StationID: "stn-0e0e0e0e0e0e0e0e", ServerTS: t0, Timezone: "America/Chicago",
			Feeds:  map[string]string{"nodes": "d-nodes"},
			Claims: map[string]string{"SYN-PROC": "d-claims"},
			BinWatch: func() *string {
				s := "d-watch"
				return &s
			}(),
			Asks:           []string{"bin.count"},
			EphemeralTopic: "syn.ephemeral",
		},
		"EdgeStale": protocol.EdgeStale{
			StationID: "stn-0e0e0e0e0e0e0e0e", Message: "no heartbeat",
		},
		"EdgeRegisterRequest": protocol.EdgeRegisterRequest{
			StationID: "stn-0e0e0e0e0e0e0e0e", Reason: "station not enrolled",
		},
		"OrderProjection": protocol.OrderProjection{
			OrderUUID: "00000000-0000-4000-8000-000000000001", OrderType: "retrieve", Status: "queued",
			StationID: "stn-0e0e0e0e0e0e0e0e", Quantity: 2, SourceNode: "SYN-SRC", DeliveryNode: "SYN-DST",
			PayloadCode: "SYN-PART-A", PayloadDesc: "synthetic part", RetrieveEmpty: true,
			OriginID: "origin-1", OriginClass: "kanban", QueueReason: "no bin", QueueCode: "no_source_bin",
			Version: 9,
		},
		"OrderStatusSnapshot": protocol.OrderStatusSnapshot{
			OrderUUID: "00000000-0000-4000-8000-000000000002", Found: true, Status: "faulted",
			StationID: "stn-0e0e0e0e0e0e0e0e", SourceNode: "SYN-SRC", DeliveryNode: "SYN-DST",
			VendorOrderID: "vendor-1", ErrorDetail: "blocked", QueueReason: "no bin", QueueCode: "no_source_bin",
			FaultSince: &t0, FaultDeadline: &t1, FaultNotice: true, FaultNoticeAfterS: 60,
			FaultRef: &protocol.TermRef{Node: "SYN-DST", Payload: "SYN-PART-A", Peer: 5, VendorCode: 52,
				VendorDesc: "obstacle", Detail: "synthetic"},
			Version: 11,
		},
		"LinesideLevelReport": protocol.LinesideLevelReport{
			Station: "stn-0e0e0e0e0e0e0e0e", ReportedAt: t0,
			Entries: []protocol.LinesideLevelEntry{{CoreNodeName: "SYN-LINE", PayloadCode: "SYN-PART-A",
				BinCount: 40, BinUOP: 5, BucketQty: 2, BinID: &binID, BinEpoch: 4, FlushedSeq: 12}},
		},
		"NodeStructureChanged": protocol.NodeStructureChanged{
			NodeID: 6, NodeName: "SYN-GROUP", OldParentID: &oldParent, NewParentID: &newParent, Action: "reparented",
		},
		"SupplyRefusalSnapshot": protocol.SupplyRefusalSnapshot{
			Digest: "d-refusals",
			Open: []protocol.SupplyRefusalState{{Action: "opened", LoaderNode: "SYN-LOADER", PayloadCode: "SYN-PART-A",
				RefusedAt: t0, RefusedBy: "stn-0e0e0e0e0e0e0e0e", AckAt: &ack, AckChoice: "wait",
				AckProcessID: "SYN-PROC"}},
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
