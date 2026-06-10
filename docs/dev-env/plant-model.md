# The Plant Model (what the sim runs)

How a declarative plant spec becomes a running Shingo plant in the sim, the
building blocks it's made of, and what the demo plant currently models.

Companion doc: [`sim.md`](sim.md) — the sim itself.

> **Status:** the modeling *framework* (§1–§3) is stable. The *current demo plant*
> (§4) and the *production model it's evolving toward* (§5) are in active
> development — being shaped to a realistic, sustained, metric-rich plant.
> Those two sections are intentionally incomplete and will be filled in as the
> model firms up.

---

## 1. From spec to plant

A plant is described declaratively in a single YAML file (e.g.
`plants/demo.yaml`), loaded + validated by `shingo-core/plantspec`, and written to
both stores by the seed tool (`shingo-core/cmd/seeddev`):

- **`seedCore`** → Postgres: node types, payloads + bin types, the storage
  hierarchy (zone → lane → slot), stations, bins (+ manifests), node↔payload
  links, and the demand registry.
- **`seedEdge`** → SQLite: processes, styles, operator stations, claims, reporting
  points, payload catalog, and the initial runtime states (so the loops bootstrap).
- **`crossValidate`** → confirms the two agree.

Everything is existence-checked, so re-seeding is idempotent.

---

## 2. Building blocks

| Block | Spec key | What it is |
|---|---|---|
| **Zone / lane / slot** | `zones[].lanes[].slots[]` | A supermarket: a zone holds lanes, a lane holds depth-ordered slots. `retrieve_algorithm` (e.g. FIFO) + `store_algorithm` (e.g. DPTH) govern access; a part buried at depth > 1 forces a reshuffle. |
| **Station** | `stations[]` | A physical node + its `kind` (press, weld, line_in, loader, unloader, staging, dest). |
| **Process** | `processes[]` | An independently-counting unit with one `active_style`. A counter tick applies to every node of the active `(process, style)`, so independently-counting nodes must be separate processes. |
| **Style** | `styles[]` | A `(process, payload)` the process can run. The repertoire a process changes over between; one is active at a time. |
| **Claim** | `claims[]` | A style→node binding: `role` (produce/consume), `swap_mode`, `payload`, `uop_capacity`, `reorder_point`, staging nodes, `auto_reorder`/`auto_push`. A/B pairs share a process+style via `paired_core_node` + `active_pull` (the parked side's ticks are skipped). |
| **Payload / Bin** | `payloads[]`, `bins[]` | A part (`uop_capacity`) and a physical container holding some UOP of a payload at a slot/node. |
| **Demand** | `demands[]` | A demand-registry entry (payload wanted at a node). Drives C-push loader replenishment via a per-(loader, payload) UOP threshold. |
| **Reporting point** | `reporting_points[]` | Ties a PLC counter tag to a node. `plc_name`/`tag_name` MUST match the edge sim process entries in `shingoedge.dev.yaml`. |
| **Lineside bucket** | `lineside_buckets[]` | Pre-staged lineside inventory a consume tick drains. |

---

## 3. The material-flow trigger model

- Each process has an **`active_style`**; the seeder sets `active_style_id` so the
  edge's `findActiveClaim` resolves a claim and ticks aren't dropped.
- A **counter tick** for a `(process, style)` is applied to every node of that
  active pair via the edge's counter-delta handling.
- **Produce** nodes accumulate toward `uop_capacity`, then finalize → swap (remove
  the full, deliver an empty). Auto-relief gates on a bound active bin.
- **Consume** nodes drain toward `reorder_point`, then reorder → retrieve a full.
  Auto-reorder gates on `auto_reorder && remaining <= reorder_point`.
- **A/B pairs** share one process+style; `active_pull` arbitrates which side counts.
- A node with **no bound active bin** holds its ticks (`no_bin_bound`) — the
  produce-side idle. (The consume side has no symmetric idle yet; in the sim, a
  starved line keeps ticking, which a realistic model should stop — see `sim.md`.)

---

## 4. What the demo currently models  *(in progress)*

> **TODO — being rebuilt.** The original demo was a small fixture that exercised
> the mechanisms but didn't sustain (it over-produced into a supermarket with no
> drain and jammed). It's being rebuilt to the realistic model in §5. Fill in the
> concrete node/part/loop inventory once that lands.

Target skeleton (single tier, build first):

```
PRESS-LH ─▶ LH-PANEL ─┐
PRESS-RH ─▶ RH-PANEL ─┤─▶  SM-STAMP  (buffer; a panel buried → reshuffle)
ASSEMBLY + CUSTOMERS
  ├─ LINE-1A/1B ◀ LH-PANEL   (in-house, A/B)
  ├─ LINE-2     ◀ RH-PANEL   (changeover repertoire)
  └─ CUSTOMER   ◀ panels     (ship-out)
```

Mechanisms exercised: produce + swap, consume + reorder, A/B pair, changeover,
reshuffle, lineside. (Loader / C-push infeed added next, with a proper
demand-threshold field rather than the current `reorder_point` conflation.)

---

## 5. The production model it's evolving toward  *(design — in progress)*

The real plant behavior the demo should reflect (to be developed further):

- **Presses** — fast, very short cycle, high volume; a repertoire of parts
  (~styles × left/right). They **run flat out and almost never idle** — the only
  real stops are **downtime** (breakdowns) and **long changeovers (30–40 min)**.
  Do not model a press as idling when the buffer is full.
- **Lines** — slower, steadier; each focused on one part, run long, occasional
  changeovers.
- **Customers** — external pulls (shipped parts); a press feeds several lines +
  customers.
- **Balance is by RATE, not 1:1 counts** — `press rate ≈ Σ(line + customer pull
  rates)`. The supermarket buffer decouples the continuous press from the steady
  draw; provision enough fan-out + buffer that the press stays fed.
- **WIP hierarchy** — two sub-lines build sub-assemblies that feed a main line
  (multi-level BOM). Needs a node that both consumes and produces.
- **Data features** (for dashboards + exec) — downtime, changeover durations,
  quality/scrap, customer demand vs actual. These are first-class signals the
  sim should emit, not just realism.

This section is the spec to develop; once concrete it folds back into §4.

---

## 6. Deferred sim signals

Features designed but not yet wired, captured here so the schema and approach
don't get lost.

### G10 — Quality/scrap events (deferred)

**Status:** Greenfield — no existing quality event anywhere in Shingo.
Deferred by Stephen 2026-06-08: "no quality event in shingo right now… let's
defer this and put it in a repo doc."

**Design sketch (from plant-model-round-2.md G10 row):**

- New event: `(cell, payload, qty, reason)`.
- Wire: Edge → Kafka (`production.quality` subject) → Core `cell_quality_events` table.
- Sim generation: seeded reject % per machine. A "scrap tick" decrements input(s)
  without crediting output — the OEE quality factor.
- Schema should accommodate a future operator HMI form emitting the same event
  (don't build the HMI now — just don't paint the schema into a sim-only corner).
- OEE: `quality = good_parts / total_parts`. Feeds the OEE product alongside
  availability (G9 downtime events) and performance (cycle time vs target).

**When to pick up:** When the team wants OEE quality factor on dashboards or
when a plant operator asks for a scrap-recording workflow.
