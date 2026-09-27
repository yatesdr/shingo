//go:build docker

package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"shingo/protocol/testutil"
	"shingocore/domain"
)

// planner_births_docker_test.go — what each door WRITES AT BIRTH, beside what it
// decided.
//
// ── WHAT IT IS FOR ────────────────────────────────────────────────────────
//
// B2 puts every Core door behind one builder, Build(ex, need, plan), over the
// single INSERT. Its proof is a differential: each door's order row as the INSERT
// wrote it, before and after. The door golden cannot show that. It records the
// orders as they stand at the END of a case, after the door dispatched inline,
// the scanner ran and admission rewrote the destination, so a row born `pending`
// at a group and one born `dispatched` at a slot can read the same there.
//
// ── HOW THE BIRTH IS CAUGHT ───────────────────────────────────────────────
//
// An AFTER INSERT trigger on orders, installed by the case into its own fresh
// database, copies each new row into golden_births. It is the INSERT's own NEW
// row, so it is exactly what the one INSERT wrote (store/orders/orders.go
// Create) and nothing written after it, with no production seam. The trigger runs
// inside the INSERT statement on the server, so the counting store sees no extra
// statement and the zero-write test and the BUDGET lines are unchanged; the door
// files read orders, bins, nodes, reservations and the outbox, never
// golden_births, so they stay byte-identical.
//
// ── WHAT IS RECORDED ──────────────────────────────────────────────────────
//
// The door golden's rules hold: names, never ids; orders by position; no vendor
// ids, uuids or timestamps. Per born row: status, the plan fields (type, source,
// delivery, process node, bin, steps, source intent, coordinated), the origin
// stamp, whether the destination-resolved clock rode the INSERT, and projection:
// the status of the first data.order.projected the door queued for that order, or
// nothing when the door does not project.

// recordBirths installs the birth trigger. Call it after newGoldenPlant and
// before the door runs.
func (g *goldenPlant) recordBirths() {
	g.t.Helper()
	g.exec(`CREATE TABLE golden_births AS SELECT * FROM orders WITH NO DATA`)
	g.exec(`CREATE FUNCTION golden_birth() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN INSERT INTO golden_births SELECT NEW.*; RETURN NEW; END $$`)
	g.exec(`CREATE TRIGGER golden_birth AFTER INSERT ON orders FOR EACH ROW EXECUTE FUNCTION golden_birth()`)
}

type birthAnswer struct {
	Index        int      `json:"index"`
	Parent       int      `json:"parent,omitempty"`
	Type         string   `json:"type"`
	Status       string   `json:"status"`
	Station      string   `json:"station,omitempty"`
	Quantity     int64    `json:"quantity"`
	Payload      string   `json:"payload,omitempty"`
	Source       string   `json:"source,omitempty"`
	Delivery     string   `json:"delivery,omitempty"`
	ProcessNode  string   `json:"process_node,omitempty"`
	Bin          string   `json:"bin,omitempty"`
	Steps        []string `json:"steps,omitempty"`
	SourceIntent string   `json:"source_intent,omitempty"`
	Coordinated  bool     `json:"coordinated,omitempty"`
	Sibling      bool     `json:"has_sibling,omitempty"`
	OriginClass  string   `json:"origin_class,omitempty"`
	OriginID     string   `json:"origin_id,omitempty"`
	ResolvedAt   bool     `json:"destination_resolved_at_birth,omitempty"`
	Projected    string   `json:"projected,omitempty"`
	// Plan is the pick the row was born with (orders.plan, v147): the bin and
	// the node it stood on.
	Plan string `json:"plan,omitempty"`
}

// births renders every row the case's doors inserted, in insert order. Holder
// orders a case made for itself are left out, as orders() leaves them out.
// Projections are read from the outbox after id `since`.
func (g *goldenPlant) births(since int) []birthAnswer {
	g.t.Helper()
	projected := g.projections(since)
	rows, err := g.db.DB.Query(`
		SELECT id, parent_order_id, edge_uuid, order_type, status, COALESCE(station_id,''), quantity,
		       COALESCE(payload_code,''), COALESCE(source_node,''), COALESCE(delivery_node,''),
		       COALESCE(process_node,''), bin_id, COALESCE(steps_json,''), COALESCE(source_intent,''),
		       coordinated, sibling_order_uuid <> '', COALESCE(origin_class,''),
		       COALESCE(origin_id::text,''), destination_resolved_at IS NOT NULL, plan
		FROM golden_births WHERE edge_uuid NOT LIKE 'golden-holder-%' ORDER BY id`)
	testutil.MustNoErr(g.t, err, "list births")
	type raw struct {
		id     int64
		parent *int64
		uuid   string
		a      birthAnswer
		steps  string
		bin    *int64
		plan   string
	}
	var all []raw
	for rows.Next() {
		var r raw
		testutil.MustNoErr(g.t, rows.Scan(&r.id, &r.parent, &r.uuid, &r.a.Type, &r.a.Status, &r.a.Station, &r.a.Quantity,
			&r.a.Payload, &r.a.Source, &r.a.Delivery, &r.a.ProcessNode, &r.bin, &r.steps, &r.a.SourceIntent,
			&r.a.Coordinated, &r.a.Sibling, &r.a.OriginClass, &r.a.OriginID, &r.a.ResolvedAt, &r.plan), "scan birth")
		all = append(all, r)
	}
	testutil.MustNoErr(g.t, rows.Err(), "births rows")
	rows.Close()

	index := map[int64]int{}
	episodes := map[string]string{}
	var out []birthAnswer
	for i, r := range all {
		index[r.id] = i + 1
		r.a.Index = i + 1
		if r.parent != nil {
			r.a.Parent = index[*r.parent]
		}
		r.a.Bin = g.labelOf(r.bin)
		r.a.Steps = renderSteps(r.steps)
		// Origin ids are uuids, some minted at random (the keeper's episode), so
		// they are named by first appearance in the case: which orders share an
		// episode is the decision, the uuid is not.
		switch {
		case r.a.OriginID == "":
		case r.a.OriginID == r.uuid:
			r.a.OriginID = "<the order's own uuid>"
		default:
			if _, ok := episodes[r.a.OriginID]; !ok {
				episodes[r.a.OriginID] = fmt.Sprintf("episode %d of this case", len(episodes)+1)
			}
			r.a.OriginID = episodes[r.a.OriginID]
		}
		r.a.Projected = projected[r.uuid]
		if r.plan != "" {
			var pick domain.PlannedPick
			testutil.MustNoErr(g.t, json.Unmarshal([]byte(r.plan), &pick), "read a birth's plan")
			r.a.Plan = g.labelOf(&pick.BinID) + " at " + pick.Node
		}
		out = append(out, r.a)
	}
	g.logBirthBudget()
	return out
}

// logBirthBudget logs, as evidence and not a pin, what a door read before its
// first INSERT INTO orders and how many rows it inserted: the statements B2's
// Build(ex, need, plan) replaces must not rise (PLAN, "Budgets").
func (g *goldenPlant) logBirthBudget() {
	stmts := g.counter.Statements()
	first, inserts := -1, 0
	for i, s := range stmts {
		if strings.HasPrefix(strings.TrimSpace(s), "INSERT INTO orders ") {
			if first < 0 {
				first = i
			}
			inserts++
		}
	}
	g.t.Logf("BIRTH %s: %d statement(s) before the first INSERT INTO orders, %d INSERT(s), case total %d",
		g.t.Name(), max(first, 0), inserts, len(stmts))
}

// projections maps an order uuid to the status of the first projection queued
// for it after outbox id `since`.
func (g *goldenPlant) projections(since int) map[string]string {
	g.t.Helper()
	rows, err := g.db.DB.Query(`SELECT payload FROM outbox WHERE id > $1 AND msg_type = 'data.order.projected' ORDER BY id`, since)
	testutil.MustNoErr(g.t, err, "projections")
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var payload []byte
		testutil.MustNoErr(g.t, rows.Scan(&payload), "scan projection")
		var v any
		_ = json.Unmarshal(payload, &v)
		uuid := findKey(v, "order_uuid")
		if _, seen := out[uuid]; uuid != "" && !seen {
			out[uuid] = findKey(v, "status")
		}
	}
	return out
}
