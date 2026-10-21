package main

import (
	"fmt"
	"net/url"
	"time"

	"shingo/protocol"
	"shingoedge/domain"
)

// door_walk_fixtures_test.go — one fixture per door the walk runs
// (door_walk_test.go). A fixture reaches its write where the walk's plant
// lets it: Core is unreachable on purpose (core.invalid), so a door that must
// ask Core first answers its refusal, and is pinned at that status.

// at is a fixed request.
func at(path string, body any) func(*walk) (string, any) {
	return func(*walk) (string, any) { return path, body }
}

// on is a request built from the walk's plant.
func on(f func(w *walk) (string, any)) func(*walk) (string, any) { return f }

func routeFixtures() map[string]routeFixture {
	sp := func(w *walk) string { return "/api/operator-stations/" + id(w.pressStation) }
	pn := func(w *walk) string { return "/api/process-nodes/" + id(w.pressNode) }
	pr := func(w *walk) string { return "/api/processes/" + id(w.press) }
	return map[string]routeFixture{
		// ── login ──
		"POST /login":  {want: 303, req: at("/login", url.Values{"username": {"walk"}, "password": {"walk-password"}})},
		"POST /logout": {want: 303, req: at("/logout", nil)},

		// ── shop floor: process-node verbs ──
		"POST /api/confirm-delivery/{orderID}": {want: 200, moves: true, req: on(func(w *walk) (string, any) {
			return "/api/confirm-delivery/" + id(w.orderID(w.freshOrder("delivered"))), map[string]any{}
		})},
		"POST /api/process-nodes/{id}/request": {want: 400, req: on(func(w *walk) (string, any) { return pn(w) + "/request", map[string]any{} })},
		// The release reaches its write at the walk's press, and announces it.
		"POST /api/process-nodes/{id}/release-empty": {want: 200, moves: true, req: on(func(w *walk) (string, any) {
			return pn(w) + "/release-empty", map[string]any{}
		})},
		"POST /api/process-nodes/{id}/release-partial": {want: 400, req: on(func(w *walk) (string, any) {
			return pn(w) + "/release-partial", map[string]any{"qty": 1}
		})},
		"POST /api/process-nodes/{id}/release-staged": {want: 400, req: on(func(w *walk) (string, any) {
			return pn(w) + "/release-staged", map[string]any{}
		})},
		"POST /api/process-nodes/{id}/keep-staged/resume": {want: 400, req: on(func(w *walk) (string, any) {
			return pn(w) + "/keep-staged/resume", map[string]any{}
		})},
		"POST /api/process-nodes/{id}/finalize": {want: 400, req: on(func(w *walk) (string, any) { return pn(w) + "/finalize", map[string]any{} })},
		"POST /api/process-nodes/{id}/load-bin": {want: 400, req: on(func(w *walk) (string, any) {
			return pn(w) + "/load-bin", map[string]any{"payload_code": "PART-W", "uop_count": 5}
		})},
		"POST /api/process-nodes/{id}/clear-bin": {want: 400, req: on(func(w *walk) (string, any) { return pn(w) + "/clear-bin", map[string]any{} })},
		"POST /api/process-nodes/{id}/record-count": {want: 400, req: on(func(w *walk) (string, any) {
			return pn(w) + "/record-count", map[string]any{"actual_uop": 3, "actor": "walk"}
		})},
		// The home clear is one request to Core and nothing else on the Edge; the
		// walk has no Core, so the door answers 502 having changed nothing.
		"POST /api/process-nodes/{id}/clear-loader-home": {want: 502, req: on(func(w *walk) (string, any) {
			return "/api/process-nodes/" + id(w.loaderNode) + "/clear-loader-home", map[string]any{}
		})},
		"POST /api/process-nodes/{id}/pull-from-market": {want: 400, req: on(func(w *walk) (string, any) {
			return pn(w) + "/pull-from-market", map[string]any{"bin_label": "BIN-1"}
		})},
		"POST /api/process-nodes/{id}/push-empty": {want: 400, req: on(func(w *walk) (string, any) {
			return "/api/process-nodes/" + id(w.loaderNode) + "/push-empty", map[string]any{}
		})},
		"POST /api/process-nodes/{id}/request-empty": {want: 400, req: on(func(w *walk) (string, any) {
			return "/api/process-nodes/" + id(w.loaderNode) + "/request-empty", map[string]any{}
		})},
		"POST /api/process-nodes/{id}/supply-refusal": {want: 200, moves: true, req: on(func(w *walk) (string, any) {
			return "/api/process-nodes/" + id(w.loaderNode) + "/supply-refusal", map[string]any{"payload_code": "PART-W", "refused_by": "Walk"}
		})},
		"DELETE /api/process-nodes/{id}/supply-refusal": {want: 200, req: on(func(w *walk) (string, any) {
			return "/api/process-nodes/" + id(w.loaderNode) + "/supply-refusal?payload_code=PART-W", map[string]any{"payload_code": "PART-W"}
		})},
		"POST /api/process-nodes/{id}/supply-refusal/ack": {want: 200, moves: true, req: on(func(w *walk) (string, any) {
			return pn(w) + "/supply-refusal/ack", map[string]any{"loader_node": "SMN_W1", "payload_code": "PART-W", "choice": "wait"}
		})},
		"POST /api/process-nodes/{id}/request-full": {want: 400, req: on(func(w *walk) (string, any) {
			return "/api/process-nodes/" + id(w.loaderNode) + "/request-full", map[string]any{}
		})},
		"POST /api/process-nodes/{id}/clear-orders": {want: 200, moves: true, req: on(func(w *walk) (string, any) { return pn(w) + "/clear-orders", map[string]any{} })},
		"POST /api/process-nodes/{id}/quality-hold": {want: 400, req: on(func(w *walk) (string, any) {
			return pn(w) + "/quality-hold", map[string]any{"actor": "walk"}
		})},

		// ── containment ──
		"POST /api/containment/release": {want: 400, req: at("/api/containment/release", map[string]any{"node_name": "PLN_01", "bin_id": 1, "actor": "walk"})},
		"POST /api/containment/recall":  {want: 400, req: at("/api/containment/recall", map[string]any{"node_name": "PLN_01", "bin_id": 1, "actor": "walk"})},
		"POST /api/containment/unhold":  {want: 400, req: at("/api/containment/unhold", map[string]any{"node_name": "PLN_01", "bin_id": 1, "actor": "walk"})},

		// ── changeover and the flow ──
		"POST /api/processes/{id}/changeover/preview": {want: 200, req: on(func(w *walk) (string, any) {
			return pr(w) + "/changeover/preview", map[string]any{"to_style_id": w.styleA}
		})},
		"POST /api/processes/{id}/changeover/start": {want: 400, req: on(func(w *walk) (string, any) {
			return pr(w) + "/changeover/start", map[string]any{"to_style_id": w.styleA, "called_by": "walk"}
		})},
		"POST /api/processes/{id}/flow/preview": {want: 200, req: on(func(w *walk) (string, any) { return pr(w) + "/flow/preview", map[string]any{} })},
		"POST /api/processes/{id}/flow/save": {want: 500, req: on(func(w *walk) (string, any) {
			return pr(w) + "/flow/save", map[string]any{"station_id": w.pressStation}
		})},
		"POST /api/processes/{id}/flow/mint": {want: 422, req: on(func(w *walk) (string, any) {
			return pr(w) + "/flow/mint", map[string]any{"station_id": w.pressStation}
		})},
		"POST /api/processes/{id}/changeover/cutover": {want: 400, req: on(func(w *walk) (string, any) {
			return pr(w) + "/changeover/cutover", map[string]any{}
		})},
		"POST /api/processes/{id}/changeover/cancel": {want: 400, req: on(func(w *walk) (string, any) {
			return pr(w) + "/changeover/cancel", map[string]any{}
		})},
		"POST /api/processes/{id}/post-cutover-flag/confirm": {want: 200, req: on(func(w *walk) (string, any) {
			return pr(w) + "/post-cutover-flag/confirm", map[string]any{}
		})},
		"POST /api/processes/{id}/changeover/stage-node/{nodeID}": {want: 400, req: on(func(w *walk) (string, any) {
			return pr(w) + "/changeover/stage-node/" + id(w.pressNode), map[string]any{}
		})},
		"POST /api/processes/{id}/changeover/evacuate-node/{nodeID}": {want: 400, req: on(func(w *walk) (string, any) {
			return pr(w) + "/changeover/evacuate-node/" + id(w.pressNode), map[string]any{}
		})},
		"POST /api/processes/{id}/changeover/deliver-material/{nodeID}": {want: 400, req: on(func(w *walk) (string, any) {
			return pr(w) + "/changeover/deliver-material/" + id(w.pressNode), map[string]any{}
		})},
		"POST /api/processes/{id}/changeover/switch-station/{stationID}": {want: 200, req: on(func(w *walk) (string, any) {
			return pr(w) + "/changeover/switch-station/" + id(w.pressStation), map[string]any{}
		})},
		"POST /api/processes/{id}/changeover/switch-node/{nodeID}": {want: 400, req: on(func(w *walk) (string, any) {
			return pr(w) + "/changeover/switch-node/" + id(w.pressNode), map[string]any{}
		})},
		"POST /api/processes/{id}/changeover/abandon-node/{nodeID}": {want: 400, req: on(func(w *walk) (string, any) {
			return pr(w) + "/changeover/abandon-node/" + id(w.pressNode), map[string]any{}
		})},
		"POST /api/processes/{id}/changeover/release": {want: 400, req: on(func(w *walk) (string, any) {
			return pr(w) + "/changeover/release", map[string]any{}
		})},

		// ── order lifecycle ──
		"POST /api/orders/{orderID}/release": {want: 200, req: on(func(w *walk) (string, any) { return "/api/orders/" + id(w.order) + "/release", map[string]any{} })},
		"POST /api/orders/{orderID}/submit":  {want: 200, req: on(func(w *walk) (string, any) { return "/api/orders/" + id(w.order) + "/submit", map[string]any{} })},
		"POST /api/orders/{orderID}/abort":   {want: 200, moves: true, req: on(func(w *walk) (string, any) { return "/api/orders/" + id(w.order) + "/abort", map[string]any{} })},

		// ── lineside ──
		"POST /api/lineside/buckets/{id}/clear": {want: 400, req: on(func(w *walk) (string, any) {
			bucket, err := w.db.CaptureLinesideBucket(w.pressNode, "PART-W", 4)
			if err != nil {
				w.t.Fatalf("capture bucket: %v", err)
			}
			return "/api/lineside/buckets/" + id(int64(bucket)) + "/clear", map[string]any{}
		})},

		// ── admin: order creation ──
		"POST /api/orders/retrieve": {want: 200, req: at("/api/orders/retrieve", map[string]any{"delivery_node": "PLN_01", "payload_code": "PART-W", "quantity": 1})},
		"POST /api/orders/move":     {want: 200, req: at("/api/orders/move", map[string]any{"source_node": "SMN_01", "delivery_node": "PLN_04", "quantity": 1})},
		"POST /api/orders/complex":  {want: 400, req: at("/api/orders/complex", map[string]any{"steps": []any{}})},
		"POST /api/orders/ingest":   {want: 400, req: at("/api/orders/ingest", map[string]any{"node": "PLN_01", "payload_code": "PART-W"})},

		// ── admin: PLCs ──
		"POST /api/plcs/read-tag":             {want: 500, req: at("/api/plcs/read-tag", map[string]any{"plc_name": "PLC-1", "tag_name": "T"})},
		"PUT /api/replenishment/cell-reorder": {want: 400, req: at("/api/replenishment/cell-reorder", map[string]any{})},

		// ── admin: processes ──
		"POST /api/processes": {want: 200, req: on(func(w *walk) (string, any) {
			return "/api/processes", map[string]any{"name": w.uniq("WALK-PROC")}
		})},
		"PUT /api/processes/{id}": {want: 200, moves: true, req: on(func(w *walk) (string, any) {
			return pr(w), map[string]any{"name": "Press A1", "description": w.uniq("renamed")}
		})},
		"DELETE /api/processes/{id}": {want: 200, moves: true, req: on(func(w *walk) (string, any) {
			m := w.api("POST", "/api/processes", map[string]any{"name": w.uniq("WALK-DEL")})
			return "/api/processes/" + id(num(w, m, "id")), nil
		})},
		"PUT /api/processes/{id}/active-style": {want: 200, req: on(func(w *walk) (string, any) {
			return pr(w) + "/active-style", map[string]any{"style_id": w.activeStyle()}
		})},
		"PATCH /api/processes/{id}": {want: 200, moves: true, req: on(func(w *walk) (string, any) {
			return pr(w), map[string]any{"flow_composer_enabled": true}
		})},
		"POST /api/processes/{id}/containment-setting": {want: 200, req: on(func(w *walk) (string, any) {
			return pr(w) + "/containment-setting", map[string]any{"enabled": false}
		})},
		"POST /api/processes/{id}/presets": {want: 400, req: on(func(w *walk) (string, any) {
			return pr(w) + "/presets", map[string]any{"name": w.uniq("preset"), "from_style_id": w.activeStyle()}
		})},
		"PATCH /api/processes/{id}/presets/{presetID}": {want: 200, req: on(func(w *walk) (string, any) {
			return pr(w) + "/presets/" + id(w.preset()), map[string]any{"name": w.uniq("renamed")}
		})},
		"POST /api/processes/{id}/presets/{presetID}/archive": {want: 200, req: on(func(w *walk) (string, any) {
			return pr(w) + "/presets/" + id(w.preset()) + "/archive", map[string]any{}
		})},
		"POST /api/processes/{id}/routing-nodes": {want: 200, req: on(func(w *walk) (string, any) {
			return pr(w) + "/routing-nodes", map[string]any{"core_node_name": "SLN_010", "role": "staging"}
		})},
		"PUT /api/processes/{id}/routing-nodes": {want: 200, req: on(func(w *walk) (string, any) {
			return pr(w) + "/routing-nodes", map[string]any{"nodes": []map[string]any{{"core_node_name": "SLN_010", "role": "staging"}, {"core_node_name": "SLN_011", "role": "staging"}}}
		})},
		"PATCH /api/processes/{id}/routing-nodes/{rowID}": {want: 200, req: on(func(w *walk) (string, any) {
			return pr(w) + "/routing-nodes/" + id(w.routingRow()), map[string]any{"enabled": false}
		})},
		"DELETE /api/processes/{id}/routing-nodes/{rowID}": {want: 200, req: on(func(w *walk) (string, any) {
			return pr(w) + "/routing-nodes/" + id(w.routingRow()), nil
		})},
		"PUT /api/processes/{id}/payloads": {want: 200, req: on(func(w *walk) (string, any) {
			return pr(w) + "/payloads", map[string]any{"payloads": []string{"PART-W"}}
		})},

		// ── admin: process groups ──
		"POST /api/process-groups": {want: 200, req: on(func(w *walk) (string, any) {
			return "/api/process-groups", map[string]any{"name": w.uniq("group")}
		})},
		"PUT /api/process-groups/{id}": {want: 200, req: on(func(w *walk) (string, any) {
			return "/api/process-groups/" + id(w.group()), map[string]any{"name": w.uniq("group")}
		})},
		"DELETE /api/process-groups/{id}": {want: 200, req: on(func(w *walk) (string, any) {
			m := w.api("POST", "/api/process-groups", map[string]any{"name": w.uniq("group")})
			return "/api/process-groups/" + id(num(w, m, "id")), nil
		})},
		"PUT /api/processes/{id}/group": {want: 200, moves: true, req: on(func(w *walk) (string, any) {
			g := w.group()
			return pr(w) + "/group", map[string]any{"group_id": g}
		})},

		// ── admin: styles and claims ──
		"POST /api/styles": {want: 200, moves: true, req: on(func(w *walk) (string, any) {
			return "/api/styles", map[string]any{"name": w.uniq("STYLE"), "process_id": w.press}
		})},
		"PUT /api/styles/{id}": {want: 200, moves: true, req: on(func(w *walk) (string, any) {
			return "/api/styles/" + id(w.styleA), map[string]any{"name": w.uniq("STYLE-A"), "process_id": w.press}
		})},
		"DELETE /api/styles/{id}": {want: 200, moves: true, req: on(func(w *walk) (string, any) {
			m := w.api("POST", "/api/styles", map[string]any{"name": w.uniq("STYLE-DEL"), "process_id": w.press})
			return "/api/styles/" + id(num(w, m, "id")), nil
		})},
		"POST /api/styles/{id}/restore": {want: 200, moves: true, req: on(func(w *walk) (string, any) {
			m := w.api("POST", "/api/styles", map[string]any{"name": w.uniq("STYLE-RST"), "process_id": w.press})
			sid := num(w, m, "id")
			w.api("DELETE", "/api/styles/"+id(sid), nil)
			return "/api/styles/" + id(sid) + "/restore", map[string]any{}
		})},
		"POST /api/styles/{id}/clone": {want: 200, moves: true, req: on(func(w *walk) (string, any) {
			return "/api/styles/" + id(w.styleA) + "/clone", map[string]any{"name": w.uniq("STYLE-CLONE")}
		})},

		// ── admin: stations ──
		"POST /api/operator-stations": {want: 200, req: on(func(w *walk) (string, any) {
			return "/api/operator-stations", map[string]any{"process_id": w.press, "name": w.uniq("Station"), "enabled": true}
		})},
		"PUT /api/operator-stations/{id}": {want: 200, moves: true, req: on(func(w *walk) (string, any) {
			return sp(w), map[string]any{"process_id": w.press, "code": "screen-a4", "name": w.uniq("Press screen"), "enabled": true}
		})},
		"POST /api/operator-stations/{id}/move": {want: 200, moves: true, req: on(func(w *walk) (string, any) {
			return sp(w) + "/move", map[string]any{"direction": "down"}
		})},
		"DELETE /api/operator-stations/{id}": {want: 200, req: on(func(w *walk) (string, any) {
			m := w.api("POST", "/api/operator-stations", map[string]any{"process_id": w.press, "name": w.uniq("Doomed"), "enabled": true})
			return "/api/operator-stations/" + id(num(w, m, "id")), nil
		})},
		"PUT /api/operator-stations/{id}/claimed-nodes": {want: 200, moves: true, req: on(func(w *walk) (string, any) {
			return sp(w) + "/claimed-nodes", map[string]any{"nodes": []string{"PLN_01", "PLN_04", "PLN_06"}}
		})},
		"POST /api/loader-boards": {want: 200, moves: true, req: on(func(w *walk) (string, any) {
			return "/api/loader-boards", map[string]any{"loader_key": "loader:walk-2", "process_id": w.loaderProc}
		})},

		// ── admin: process nodes ──
		"POST /api/process-nodes": {want: 200, moves: true, req: on(func(w *walk) (string, any) {
			n := w.uniq("WN")
			return "/api/process-nodes", map[string]any{"process_id": w.press, "operator_station_id": w.pressStation,
				"core_node_name": "SLN_06", "code": n, "name": n, "sequence": 9, "enabled": true}
		})},
		"PUT /api/process-nodes/{id}": {want: 200, moves: true, req: on(func(w *walk) (string, any) {
			return pn(w), map[string]any{"process_id": w.press, "operator_station_id": w.pressStation,
				"core_node_name": "PLN_01", "code": "PLN_01", "name": w.uniq("PLN_01"), "sequence": 1, "enabled": true}
		})},
		"POST /api/process-nodes/{id}/curtain-setting": {want: 200, moves: true, req: on(func(w *walk) (string, any) {
			return pn(w) + "/curtain-setting", map[string]any{"enabled": true, "plc_name": "PLC-1", "tag_name": "CURTAIN_1", "safe_value": true}
		})},

		// ── admin: syncs, shifts, config, backups, diagnostics ──
		"POST /api/core-nodes/sync":           {want: 200, req: at("/api/core-nodes/sync", nil)},
		"POST /api/payload-catalog/sync":      {want: 200, req: at("/api/payload-catalog/sync", nil)},
		"PUT /api/shifts":                     {want: 200, req: at("/api/shifts", []map[string]any{{"shift_number": 1, "name": "First", "start_time": "06:00", "end_time": "14:00"}})},
		"PUT /api/config":                     {want: 400, req: at("/api/config", map[string]any{})},
		"POST /api/config/core-api/test":      {want: 200, req: at("/api/config/core-api/test", map[string]any{"core_api": "http://core.invalid"})},
		"POST /api/config/kafka/test":         {want: 400, req: at("/api/config/kafka/test", map[string]any{"brokers": []string{}})},
		"POST /api/config/password":           {want: 400, req: at("/api/config/password", map[string]any{"current": "wrong", "new": "x"})},
		"POST /api/backups/test":              {want: 501, req: at("/api/backups/test", map[string]any{})},
		"POST /api/backups/run":               {want: 501, req: at("/api/backups/run", nil)},
		"POST /api/backups/restore":           {want: 501, req: at("/api/backups/restore", map[string]any{"key": "none"})},
		"POST /api/manual-message":            {want: 400, req: at("/api/manual-message", map[string]any{"subject": "", "payload": map[string]any{}})},
		"POST /api/diagnostics/outbox/replay": {want: 400, req: at("/api/diagnostics/outbox/replay", map[string]any{"id": 987654321})},
		"POST /api/diagnostics/orders/sync":   {want: 400, req: at("/api/diagnostics/orders/sync", nil)},
	}
}

func subjectFixtures() map[string]subjectFixture {
	none := func(typ string) subjectFixture {
		// Outbound-only types: the Edge sends these and registers a no-op so
		// its table is whole. Walked all the same.
		return subjectFixture{env: func(w *walk) *protocol.Envelope { return w.coreEnv(typ, map[string]any{}) }}
	}
	return map[string]subjectFixture{
		protocol.TypeOrderRequest:        none(protocol.TypeOrderRequest),
		protocol.TypeOrderCancel:         none(protocol.TypeOrderCancel),
		protocol.TypeOrderReceipt:        none(protocol.TypeOrderReceipt),
		protocol.TypeOrderRedirect:       none(protocol.TypeOrderRedirect),
		protocol.TypeComplexOrderRequest: none(protocol.TypeComplexOrderRequest),
		protocol.TypeOrderRelease:        none(protocol.TypeOrderRelease),
		protocol.TypeOrderIngest:         none(protocol.TypeOrderIngest),

		protocol.TypeOrderAck: {moves: true, env: func(w *walk) *protocol.Envelope {
			o := w.freshOrder("submitted")
			return w.coreEnv(protocol.TypeOrderAck, protocol.OrderAck{OrderUUID: o, ShingoOrderID: 77, SourceNode: "SMN_01"})
		}},
		protocol.TypeOrderWaybill: {moves: true, env: func(w *walk) *protocol.Envelope {
			o := w.freshOrder("acknowledged")
			return w.coreEnv(protocol.TypeOrderWaybill, protocol.OrderWaybill{OrderUUID: o, WaybillID: "WB-1", ETA: time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)})
		}},
		protocol.TypeOrderUpdate: {moves: true, env: func(w *walk) *protocol.Envelope {
			// The same status, a new reason: no status event, only the nudge.
			o := w.freshOrder("queued")
			return w.coreEnv(protocol.TypeOrderUpdate, protocol.OrderUpdate{OrderUUID: o, Status: string(protocol.StatusQueued),
				QueueReason: w.uniq("Waiting for material"), QueueCode: "waiting_for_material"})
		}},
		protocol.TypeOrderDelivered: {moves: true, env: func(w *walk) *protocol.Envelope {
			o := w.freshOrder("in_transit")
			return w.coreEnv(protocol.TypeOrderDelivered, protocol.OrderDelivered{OrderUUID: o, DeliveredAt: time.Now().UTC(), DeliveryNode: "PLN_01"})
		}},
		protocol.TypeOrderError: {moves: true, env: func(w *walk) *protocol.Envelope {
			o := w.freshOrder("submitted")
			return w.coreEnv(protocol.TypeOrderError, protocol.OrderError{OrderUUID: o, ErrorCode: "no_source", Detail: "nothing to fetch"})
		}},
		protocol.TypeOrderCancelled: {moves: true, env: func(w *walk) *protocol.Envelope {
			o := w.freshOrder("queued")
			return w.coreEnv(protocol.TypeOrderCancelled, protocol.OrderCancelled{OrderUUID: o, Reason: "walk"})
		}},
		protocol.TypeOrderStaged: {moves: true, env: func(w *walk) *protocol.Envelope {
			o := w.freshOrder("in_transit")
			return w.coreEnv(protocol.TypeOrderStaged, protocol.OrderStaged{OrderUUID: o, Detail: "staged"})
		}},
		protocol.TypeOrderSkipped: {moves: true, env: func(w *walk) *protocol.Envelope {
			o := w.freshOrder("queued")
			return w.coreEnv(protocol.TypeOrderSkipped, protocol.OrderSkipped{OrderUUID: o, ErrorCode: "bin_gone", Detail: "gone"})
		}},

		"data:" + protocol.SubjectEdgeRegistered: {env: func(w *walk) *protocol.Envelope {
			return w.dataEnv(protocol.SubjectEdgeRegistered, protocol.EdgeRegistered{StationID: "walk.edge", Message: "ok"})
		}},
		"data:" + protocol.SubjectEdgeHeartbeatAck: {env: func(w *walk) *protocol.Envelope {
			return w.dataEnv(protocol.SubjectEdgeHeartbeatAck, protocol.EdgeHeartbeatAck{StationID: "walk.edge", ServerTS: time.Now().UTC()})
		}},
		"data:" + protocol.SubjectNodeListResponse: {moves: true, env: func(w *walk) *protocol.Envelope {
			// The loader's replenishment changed: the loader tile reads it.
			return w.dataEnv(protocol.SubjectNodeListResponse, protocol.NodeListResponse{
				Nodes:   []protocol.NodeInfo{{Name: "SMN_W1"}, {Name: "SMN_W2"}, {Name: "PLN_01"}, {Name: "PLN_04"}, {Name: "PLN_06"}},
				Loaders: walkLoaders("threshold"), Digest: w.uniq("digest")})
		}},
		"data:" + protocol.SubjectCatalogPayloadsResponse: {moves: true, env: func(w *walk) *protocol.Envelope {
			return w.dataEnv(protocol.SubjectCatalogPayloadsResponse, protocol.CatalogPayloadsResponse{
				Payloads: []protocol.CatalogPayloadInfo{{ID: 9001, Name: "Walk part", Code: "PART-W", UOPCapacity: 40}}, Digest: w.uniq("cat")})
		}},
		"data:" + protocol.SubjectOrderStatusResponse: {moves: true, env: func(w *walk) *protocol.Envelope {
			return w.dataEnv(protocol.SubjectOrderStatusResponse, protocol.OrderStatusResponse{Orders: []protocol.OrderStatusSnapshot{{
				OrderUUID: w.orderUUID, Found: true, Status: string(protocol.StatusQueued), QueueReason: w.uniq("Waiting for a slot"),
				QueueCode: "dropoff-occupied"}}})
		}},
		"data:" + protocol.SubjectEdgeRegisterRequest: {env: func(w *walk) *protocol.Envelope {
			return w.dataEnv(protocol.SubjectEdgeRegisterRequest, protocol.EdgeRegisterRequest{Reason: "walk"})
		}},
		"data:" + protocol.SubjectEdgeStale: {env: func(w *walk) *protocol.Envelope {
			return w.dataEnv(protocol.SubjectEdgeStale, protocol.EdgeStale{Message: "walk"})
		}},
		"data:" + protocol.SubjectNodeStructureChanged: {env: func(w *walk) *protocol.Envelope {
			return w.dataEnv(protocol.SubjectNodeStructureChanged, protocol.NodeStructureChanged{NodeName: "PLN_01", Action: "moved"})
		}},
		"data:" + protocol.SubjectBinPickedUp: {env: func(w *walk) *protocol.Envelope {
			o := w.freshOrder("in_transit")
			return w.dataEnv(protocol.SubjectBinPickedUp, protocol.BinPickedUp{OrderUUID: o, BinID: 41, Location: "PLN_01", PickedUpAt: time.Now().UTC()})
		}},
		"data:" + protocol.SubjectBinLanded: {env: func(w *walk) *protocol.Envelope {
			return w.dataEnv(protocol.SubjectBinLanded, protocol.BinLanded{BinID: 42, CoreNodeName: "PLN_04", Count: 12, Epoch: 1,
				PayloadCode: "PART-W", PayloadKnown: true, PlacedSeq: int64(w.seq + 1)})
		}},
		"data:" + protocol.SubjectBinReturn: {env: func(w *walk) *protocol.Envelope {
			return w.dataEnv(protocol.SubjectBinReturn, protocol.BinReturn{OrderUUID: w.orderUUID, BinLabel: "BIN-9", State: "returned",
				Destination: "SMN_01", At: time.Now().UTC()})
		}},
		"data:" + protocol.SubjectUOPAdjustment: {env: func(w *walk) *protocol.Envelope {
			return w.dataEnv(protocol.SubjectUOPAdjustment, protocol.UOPAdjustment{BinID: 42, CoreNodeName: "PLN_04", NewRemaining: 7 + w.seq,
				Actor: "walk", AdjustedAt: time.Now().UTC(), Epoch: 1})
		}},
		"data:" + protocol.SubjectBinEpochRefresh: {env: func(w *walk) *protocol.Envelope {
			return w.dataEnv(protocol.SubjectBinEpochRefresh, protocol.BinEpochRefresh{BinID: 42, CoreNodeName: "PLN_04", Epoch: 2})
		}},
		"data:" + protocol.SubjectSourcingState: {moves: true, env: func(w *walk) *protocol.Envelope {
			return w.dataEnv(protocol.SubjectSourcingState, protocol.SourcingStateReport{States: []protocol.SourcingState{{
				ProcessID: "Press A1", StyleID: id(w.styleA), Status: "red", Reason: w.uniq("short"), ComputedAt: time.Now().UTC()}}})
		}},
		"data:" + protocol.SubjectSupplyRefusalState: {env: func(w *walk) *protocol.Envelope {
			return w.dataEnv(protocol.SubjectSupplyRefusalState, protocol.SupplyRefusalState{Action: protocol.SupplyRefusalOpened,
				LoaderNode: "SMN_W1", PayloadCode: "PART-W", RefusedAt: time.Now().UTC(), RefusedBy: "Another Edge"})
		}},
		"data:" + protocol.SubjectSupplyRefusalSnapshot: {env: func(w *walk) *protocol.Envelope {
			return w.dataEnv(protocol.SubjectSupplyRefusalSnapshot, protocol.SupplyRefusalSnapshot{Digest: w.uniq("ref")})
		}},
		"data:" + protocol.SubjectOrderProjected: {moves: true, env: func(w *walk) *protocol.Envelope {
			return w.dataEnv(protocol.SubjectOrderProjected, protocol.OrderProjection{OrderUUID: w.uniq("projected"),
				OrderType: protocol.OrderTypeMove, Status: string(protocol.StatusQueued), StationID: "walk.edge", Quantity: 1,
				SourceNode: "SMN_01", DeliveryNode: "PLN_06"})
		}},
		"data:" + protocol.SubjectContainmentSnapshot: {env: func(w *walk) *protocol.Envelope {
			now := time.Now().UTC()
			return w.dataEnv(protocol.SubjectContainmentSnapshot, protocol.ContainmentSnapshot{Digest: w.uniq("cont"),
				Flags: []protocol.PayloadContainmentRow{{PayloadCode: "PART-W", Active: true, Reason: "walk", ActivatedAt: &now}}})
		}},
		"data:" + protocol.SubjectBinsSnapshot: {env: func(w *walk) *protocol.Envelope {
			return w.dataEnv(protocol.SubjectBinsSnapshot, protocol.NodeBinsSnapshot{Digest: w.uniq("bins"),
				Rows: []protocol.NodeBinsRow{{Node: "PLN_01", Occupied: true, BinID: 43, BinLabel: w.uniq("BIN"), PayloadCode: "PART-W", Stocked: true}}})
		}},
	}
}

// ── plant helpers for fixtures ──

func (w *walk) activeStyle() int64 {
	w.t.Helper()
	p, err := w.db.GetProcess(w.press)
	if err != nil || p.ActiveStyleID == nil {
		w.t.Fatalf("press has no active style: %v", err)
	}
	return *p.ActiveStyleID
}

// freshOrder makes an order on the press station's first node, in status, and
// returns its uuid.
func (w *walk) freshOrder(status string) string {
	w.t.Helper()
	uuid := w.uniq("walk-order")
	oid, err := w.db.CreateOrder(uuid, protocol.OrderTypeRetrieve, &w.pressNode, false, 1, "PLN_01", "", "SMN_01", "standard", false, "PART-W", "", "")
	if err != nil {
		w.t.Fatalf("create order: %v", err)
	}
	if _, err := w.db.Exec(`UPDATE orders SET status=? WHERE id=?`, status, oid); err != nil {
		w.t.Fatalf("set order status: %v", err)
	}
	w.prepped = true
	return uuid
}

func (w *walk) orderID(uuid string) int64 {
	w.t.Helper()
	o, err := w.db.GetOrderByUUID(uuid)
	if err != nil {
		w.t.Fatalf("order %s: %v", uuid, err)
	}
	return o.ID
}

func (w *walk) routingRow() int64 {
	w.t.Helper()
	w.routing++
	name := fmt.Sprintf("SLN_0%d", w.routing)
	w.api("POST", "/api/processes/"+id(w.press)+"/routing-nodes", map[string]any{"core_node_name": name, "role": "staging"})
	rows, err := w.db.ListRoutingNodes(w.press)
	if err != nil || len(rows) == 0 {
		w.t.Fatalf("no routing row: %v", err)
	}
	return rows[len(rows)-1].ID
}

// preset stores a one-cell flow preset straight to the table (the door that
// names one from a style asks the style's flow to be whole first).
func (w *walk) preset() int64 {
	w.t.Helper()
	pid, err := w.db.CreateFlowPreset(domain.FlowPresetInput{ProcessID: w.press, Name: w.uniq("preset"), FlowJSON: `{"cells":[{"core_node_name":"PLN_01","swap_mode":"simple"}]}`, CreatedBy: "walk"})
	if err != nil {
		w.t.Fatalf("create preset: %v", err)
	}
	w.prepped = true
	return pid
}

func (w *walk) group() int64 {
	w.t.Helper()
	m := w.api("POST", "/api/process-groups", map[string]any{"name": w.uniq("group")})
	return num(w, m, "id")
}
