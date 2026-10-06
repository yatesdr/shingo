## Domain glossary

Names matter — drift in component naming follows drift in domain naming.
This glossary is the source of truth; use these names in code, templates,
and UI labels.

Each entry was verified against the code (citations below). Where the codebase
uses inconsistent names for the same concept today, the entry says which name
wins and the inconsistency is flagged in **Cross-surface terminology to
reconcile** at the end of this section.

### Production hierarchy

| Term | Definition | Code reference |
|---|---|---|
| **Process** | A production sequence configured for a cell (e.g. "Front Rail"). Has one ActiveStyleID and many Styles. Owns the production counter config. **One Process is active per cell at a time** (Process has `ActiveStyleID`; cell switches active style via changeover) | `shingo-edge/domain/process.go:32` `type Process struct` |
| **Style** | A variant produced under a Process (e.g. "Style A", "Style B"). Belongs to one Process via `ProcessID`. The active Style drives which NodeClaims are in effect. Also written **"Job Style"** in UI labels and changeover docs — both names are acceptable; `Style` is the code identifier, "Job Style" is fine in operator-facing text | `shingo-edge/domain/process.go:41` `type Style struct` |
| **NodeClaim** | A per-Style binding to a Core Node — declares the payload, capacity, reorder behaviour, swap mode, staging. The active Style's NodeClaims drive material orders. **One Claim type exists** (the verb "claims" is used in unrelated relationships — see Claim disambiguation below) | `shingo-edge/domain/process.go:116` `type NodeClaim struct` |
| **Claim Role** | What a node does for a payload under a NodeClaim. Two live values: `consume` (node consumes upstream material), `produce` (node produces material for downstream). **Deprecated:** `changeover` — present in `protocol/types.go:235` and referenced in `engine/changeover.go` and `operator_node_changeover.go`, but does **not** reflect how changeovers actually work (its third reference, the admin page's `processes.js`, went with the claim editor in U9d). Actual changeover mechanic: operator selects a new Style → active NodeClaims change → each claim's `swap_mode` drives add/drop commands per node. No separate "changeover role" needed. Slated for removal — see deprecations tracker | `protocol/types.go:230-235` |
| **Swap Mode** | How a node's bin gets replaced. Active values: `sequential`, `single_robot`, `two_robot`, `two_robot_press_index`, `manual_swap`. **Deprecated:** `simple` (hidden in UI, legacy data still has it — see deprecations tracker) | `protocol/swap_mode.go:17-22` |

### Node concepts

| Term | Definition | Code reference |
|---|---|---|
| **Core Node** | A physical, robot-addressable location in the cell (lane, slot, station). Owned by Core, identified by a stable name string (e.g. `LANE_03_SLOT_2`). Exists whether any Edge process uses it or not. Edge receives the list via sync from Core | Referenced everywhere as `CoreNodeName string` |
| **Process Node** | An Edge-side record that says "Process X uses Core Node Y in role Z." Has its own ID, references a Core Node by name (`CoreNodeName`), carries process-scoped config (owning operator station, sequence, display name) plus a separate `RuntimeState` row (active bin, remaining UOP, active orders). Many Process Nodes can reference one Core Node (different processes sharing the same physical slot) | `shingo-edge/domain/process.go:53` `type Node struct` (the comment on line 49 explicitly says "process node") |

Person/Employee analogy: Core Node is the human (one per body), Process Node is the employment record (many possible per person, each carrying per-employer context).

### Edge installation vs HMI

This is the worst overloading in the codebase today — the word "Station" means two different things at two different scales. The reconciliation table at the end of this section lists the rename targets.

| Term | Definition | Code reference |
|---|---|---|
| **Edge Cell** | One Edge installation — a physical production cell with its own Edge instance, controllers, HMIs, and Core sync. Identified by `StationID` in `Config.Messaging`. Core's `NodeType` code `EDGE` and `Order.StationID` refer to this concept. **The term "Edge Cell" is the proposed unified name** — code currently uses "Station" (Edge config), "edge-station" (Core docstrings), and `StationID` (both) | `shingo-edge/config/...` `StationID`; `shingo-core/domain/order.go:23` `StationID`; `shingo-core/domain/node_type.go:6` "EDGE (edge station)" |
| **Loader station** | The Core Nodes page's word for a bin loader or unloader (one `bin_loaders` row; a two-stage unloader is one station made of two loaders). UI text says **station** only on that page's box and card (**+ Station**, **New station**, **Delete station**); code says loader. A third meaning of "station" — see the reconciliation table | `shingo-core/store/loaders` `Loader`; `shingo-core/www/static/pages/loaders.js` |
| **Operator Station** | A specific HMI screen inside an Edge Cell. Configured to claim a subset of the cell's Process Nodes; renders an operator-facing UI for those nodes. Multiple Operator Stations exist per cell. **In code, "Operator Station" wins** — code currently mixes this with "Station" (the domain type). **In the UI it is a screen** (*Operator screens*, *Add operator screen*, *Screen* as a column head), which keeps "station" for the cell and the loader station | `shingo-edge/domain/station.go:8` `type Station struct`; API at `/api/operator-stations`; URL `/operator/station/{id}` |

### Carriers

| Term | Definition | Code reference |
|---|---|---|
| **Bare bin type** | A bin type with `bare_of` set: the marker Core stamps on a CLEAR at a stage-1 window of a two-stage unloader. Derived from the cart's own type, one marker per cart type, created the first time that type comes through. It means "this cart holds no bin yet". A bare cart is never handed out as an empty, and a bare type is never in a payload's carrier rules or offered in any picker. At stage 2 the one-tap **Send on** posts a blank code and Core stamps the cart back to its `bare_of` type. No operator surface shows a marker code; the Edge carries only `leaves_bare`. UI: a **bare** badge on the bin types table, read-only | `shingo-core/domain/bin_type.go` `BinType.Bare`; `bin_types.bare_of`; `ensureBareMarker`; `store/bins/bins.go` `EmptyCarrierWhere` |
| **Cart** | The operator-facing word for the carrier of a two-stage unloader. HMI copy on the two-stage panels says cart, and so do the Core station box slots about it (*Carts with an empty bin go to*, *Carts wait at*); code and other admin surfaces say carrier | `operator-render.js` `confirmFullOff`, `confirmSendOn`; `loaders.js` `stationSlots` |
| **Bare position** | **Not the same thing.** An Edge word for a position with no carrier standing on it at all. Nothing to do with a bin type; do not shorten "bare bin type" to "bare" where the two could be confused | `shingo-edge/engine/consume_plan.go`, `operator_produce.go` |

### Orders

| Term | Definition | Code reference |
|---|---|---|
| **Order** | A material-movement request between nodes. The canonical noun across the system. Edge produces orders driven by demand wiring; Core receives and dispatches them. Edge URL is `/orders` (renamed from `/kanbans`; a 301 redirect preserves old bookmarks). No `Kanban` data type exists. | `shingo-edge/domain/order.go`; Edge handler `handleOrders` calls `OrderService().ListActiveByProcess()` |
| **Manual Order** | An admin-created one-off order. On Edge, submitted via the `/manual-order` page (types: `move`, `retrieve`, `store`, `complex`). On Core, submitted via Core's `/orders` admin modal (subtypes: `transport`, `staged`, `swap`, `send_to_location`), historically called "Spot Order" — Core rename to "Manual Order" is outstanding. Flows to Core via the protocol like any other Edge-originated order | Edge: `shingo-edge/www/handlers_manual_order.go`; Core: `shingo-core/www/handlers_orders.go:203` (`apiSpotOrderSubmit` — rename pending) |
| **Test Order** | A developer/QA tool on Core's `/test-orders` page for exercising order paths during development. Not an operator-facing concept | `shingo-core/www/handlers_test_orders.go`; don't use this term in operator UI |

**Spot Order vs Manual Order are not the same thing** even though they cover the same admin-need category. Different surfaces, different forms, different type vocabularies. See reconciliation table below for the proposed unified term.

### Claim disambiguation (one noun, two unrelated verb uses)

The word "claim" appears in three places. Only one of them is a data type:

1. **NodeClaim** (data type) — per-Style binding to a Core Node. The configured "Style X wants payload Y at node Z."
2. **Operator Station claims nodes** (verb / many-to-many relationship) — operator-station → claimed-nodes (`apiSetStationClaimedNodes`). Says "this HMI is responsible for these physical nodes." No `Claim` table; just an assignment.
3. **Robot claims bin** (runtime fleet concept) — a robot taking ownership of a bin for transport. Not in the process domain at all.

When writing about "claims," qualify which one. "NodeClaim" for the noun; "station node assignment" or "robot-bin ownership" for the verbs.

### Cross-surface terminology to reconcile

These are same-concept-different-name drifts where the system should pick one
name and migrate. Listed in rough order of impact × ease.

| Concept | Names today | Proposed unified name | Rename mechanics |
|---|---|---|---|
| **Edge installation / cell** | "Station" (Edge config UI, `StationID`), "edge-station" (Core docstrings), `EDGE` (Core NodeType code) | **"Edge Cell"** in UI labels and new docs. `StationID` field name stays in code (too disruptive to rename a serialized field across the protocol), but its meaning is "Edge Cell ID" | Update Edge config UI labels: "Station ID" → "Edge Cell ID". Update Core docstrings. Don't rename `StationID` in JSON/structs |
| **Loader or unloader on Core's Nodes page** | "station" (the station box and card: **+ Station**, **New station**), "loader" (code, API, every other page) | Keep **station** on that page only; a plant person thinks "the press 6 unload station", and "loader" reads as the thing that fills. Qualify it (**loader station**) anywhere it could meet the two meanings above | Nothing to rename; don't spread "station" for a loader beyond the Nodes page |
| **HMI screen inside a cell** | "Station" (domain type `shingo-edge/domain/station.go`), "OperatorStation" (API endpoint, JSON field), "screen" (Edge UI) | **"Operator Station"** in code (matches existing API). In UI labels, **screen** — *Operator screens* is the Processes page's tab and editor | Data types and APIs already match; UI strings that still say "station" for an HMI move to "screen" as their files are touched |
| **Order list page on Edge** | **Done.** URL `/orders`, page identifier `"orders"`, handler `handleOrders`. A 301 redirect from `/kanbans` preserves old bookmarks. HTMX targets use `/orders/partial`. | — (completed) | — |
| **Admin-created one-off order** | Edge: "Manual Order" (types move/retrieve/store/complex). Core: still "Spot Order" (subtypes transport/staged/swap/send_to_location) — rename to "Manual Order" is outstanding. | **"Manual Order"** — clearer than "Spot," and Edge's term is the broader one. Core's `/orders` admin modal should be renamed to "Manual Order." Subtype vocabularies stay distinct because they represent genuinely different operations | Rename Core's `apiSpotOrderSubmit` to `apiManualOrderSubmit`. Rename `.spot-tabs` CSS to `.manual-order-tabs`. Update Core nav label "Spot Order" → "Manual Order" |
| **What Core calls "edge-station" in NodeType** | `EDGE` NodeType code described as "edge station" | Keep the code as `EDGE` (short codes are intentional). Rename the human description to "edge cell" | One docstring change on `shingo-core/domain/node_type.go:6` |

Reconciliation is opportunistic adjacency — bundle these into the consistency refactor PRs as files get touched. They're not blockers; they're cleanup.

### Units and casing

| Term | Definition | Casing rule |
|---|---|---|
| **UoP** | Units of Production — the count of finished parts a bin/payload carries, or that a cell has consumed. The atomic quantity the threshold monitor sums and reorder thresholds fire on. | Always **"UoP"** in UI text — labels, headers, table columns, prose, toasts, tooltips. Never "UOP" or "uop". **Display text only:** code identifiers, JSON keys, struct fields, and `data-*` attributes keep their existing casing (`UOPRemaining`, `uop_remaining`, `data-uop`) — renaming a serialized field is out of scope and would break the protocol. |

**Casing history — and a warning about how it was mis-measured twice.**

`752dec99` (2026-07-22) swept nine files and **held perfectly**: not one of them
has regressed. `8272aac0`'s follow-up (2026-07-26) finished the job across the
files that sweep never covered. What went wrong in between was the *measurement*,
not the code, and it went wrong the same way twice. (This paragraph first cited
`2c0d3c48` — the `--sub-*` substrate-ramp commit from the same merge. A wrong
SHA inside the passage about getting the record wrong; corrected here.)

**Raw `UOP` counts are meaningless here.** Most occurrences in the tree are
supposed to be uppercase — `{{.UOPRemaining}}`, `UOPCapacity`, `remainingUOP`,
`lsUOP`, `data-uop`, `bin_uop_ledger`. An unfiltered grep returns ~199 and reads
as catastrophic drift; a differently-scoped one returned "34 vs 7" and then
"44 vs 30", which reads as a rule actively decaying. **Neither number described
a real problem**, and the second was used to justify re-opening an item that had
in fact held. Never quote an unfiltered count for this term.

The only question that means anything is: **which rendered text says `UOP`?**
Sort every hit into a bucket before touching anything:

| Bucket | Verdict |
|---|---|
| Identifier / field ref / `data-*` attribute | Correct. Leave it. Renaming a serialized field breaks the protocol |
| Rendered text in a file a previous sweep covered | A real regression |
| Rendered text in a file no sweep ever covered | Not drift — an unswept surface |
| Prose in a comment | Not the product, but it is where the next implementer copies the casing from |

Applied to the 2026-07-26 pass, all 19 rendered hits were in the **third**
bucket. `752dec99` enumerated templates plus three Core page scripts; the
**entire Operator HMI** (`shingo-edge/www/static/operator-station/`) and Edge's
`static/js/pages/` were never in its scope. The highest-visibility surface in
the system — what an operator reads all shift — had simply never been swept.
Comments were swept in the second pass on purpose, reversing the first pass's
call, on the grounds above.

**The discriminator, if you write a guard.** A test that bans the string `UOP`
fails against a *correct* tree, because it cannot tell a label from a field ref.
The predicate that can is a word-boundary match:

```
(?<![A-Za-z0-9_])UOP(?![A-Za-z0-9_])
```

Verify it **both ways** before trusting it — it must fire on `>UOP Remaining<`
and `' UOP)'`, and stay silent on `{{.UOPRemaining}}`, `data-uop-capacity`,
`remainingUOP` and `MinHysteresisUOP`. A guard that cannot separate those two
lists is not a guard. No such test exists yet.

**The checkable end state:** that pattern returns nothing in any `.js` / `.css` /
`.html` under `shingo-core/www`, `shingo-edge/www` or `shared/`. Verifiable in
ten seconds, unlike a count in a table.

### Words on the setup screens

One word per thing on the Edge Processes page and the station composer. A
field keeps the name its claim gives it (Desktop composer patterns › "A field
is called what the claim calls it"); these are the nouns around the fields.

| Say | Never | Why |
|---|---|---|
| **part** | payload (in UI text) | the plant's word on every surface, from the picker to the finding pill. `payload_code` stays the field name in code and on the wire; field labels and refusals say *part* (*Auto request part*; a claim with none is asked for a part) |
| **flow** | — | what one part does on this process: which positions it works, how each swaps, where its bins come from and go |
| **preset** | — | a named **shape** of flow. A preset is never a part and never names one; parts are applied to it |
| **screen** | station, for an HMI | see Operator Station above |
| **1‑robot swap**, **2‑robot swap**, **2‑robot index**, **Sequential A/B** | Single-robot swap, Two-robot swap, 2-Robot Press Index | the chip's word, everywhere, server refusals included. Go's `SwapModeWord` and `composer-model.js`'s `MODES` are the one table (pinned, see Drift detection) and keep its non-breaking hyphen |
| **Add a position** | — | one phrase on every door that turns a position on in a flow (the table's *+ Add a position*, the free-position menu, the empty bar's *Add a position below, or use a preset*) |
| a node name, in full | `PLN_01` → `P01` | the plant named its nodes; a shortened name is a second name nobody can search for. Too long for its box, it is cut in the middle with the full name in the tooltip |

The changeover picker's red pill says what Core answered — **No bin at
Core** (at least one part has no available bin), with Core's own note
(`missing PANEL-A`) on the row and as the pill's title — and never "No
parts". A style Core has not checked reads **Not checked at Core**. A pill
that says more than its source said is a pill that sends someone looking for
the wrong problem.

### State words

What a process or a station is doing is **three words, derived, never
stored**:

| Words | When |
|---|---|
| **Changing over** | the process's `target_style_id` is set and differs from its `active_style_id` |
| **Running `<style>`** | otherwise, when a style is running (the station view's `current_style`; the process list's running style) |
| **No part running** | neither |

Changing over is tested first: during a changeover the old style is still the
running one, and "Running Style A" would hide the changeover. Both readers —
the station's `stationState` (`operator-render.js`: header, style chip,
footer badge, empty grid) and the Processes list's State column
(`processes-desktop.js`) — derive from fields their payloads already carry,
and each surface reads its one derivation so its places cannot disagree.

**Never store a state you can derive.** `processes.production_state` held the
same fact as the two style pointers, written at changeover start, cutover and
cancel; a Settings save could write a stale copy back over the live one. It
was dropped (Edge migration v16) along with `operator_stations.device_mode`,
which had no reader and two defaults that disagreed. Before adding a state
column or payload field, check whether the words can be computed from what is
already sent; if they can, compute them, in one function per surface.

### Working principle

The glossary is the source of truth. Where the UI uses an inconsistent name
today, the inconsistency is a defect to fix during migration. **When the
system as a whole means one thing, both surfaces should call it the same
thing** — there's no legitimate reason for Core to say "spot order" while Edge
says "manual order," or for "Station" to mean two different things at two different
scales. Each row in the reconciliation table is a small consistency win
available to anyone touching the relevant file.

