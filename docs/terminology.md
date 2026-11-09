# Terminology Reference

Verified against: b9210bce

The canonical glossary: one statement per term, in Shingo's vendor-neutral words, mapped to the equivalent in common
fleet management systems where one exists. Other docs link here for a definition; where a term has a mechanism,
the entry links the doc that owns it.

## Core Concepts

### Node

A physical location in the facility where bins can be stored, picked up, or delivered, or a synthetic grouping of
such locations. A node has a name (the one the fleet backend knows it by), an optional type, a zone, an optional
parent and, for a lane slot, a depth (`nodes`; [data-model.md](data-model.md#node)). It has no capacity column:
what a node may hold is decided by its type and parent (a storage slot holds one bin).

| System | Term |
|---|---|
| Shingo | **Node** |
| Seer RDS | Bin Location (`GeneralLocation` class in scene data) |
| MiR | Position / Station |
| Locus Robotics | Location |
| 6 River Systems | Destination |
| VDA 5050 | Node (same) |

Node types are `NGRP` (node group), `LANE` (lane) and `STOR` (standalone storage slot); other physical nodes may
carry no type. Naming conventions are [node-naming-standards.md](node-naming-standards.md)'s.

**Core Node** — a node as Core owns it: a physical, robot-addressable location (lane slot, station, staging spot),
identified by its name string. It exists whether any Edge process uses it or not; the Edge receives the list from
Core and refers to one by `CoreNodeName`.

### Bin

A physical container that holds materials and is tracked as it moves between nodes. The bin is the primary physical
entity: it carries its type, status, and payload assignment (payload code, manifest, UOP remaining) directly. Its
columns are [data-model.md](data-model.md#bin)'s.

| System | Term |
|---|---|
| Shingo | **Bin** |
| Seer RDS | Goods / Container (`goodsId`, `containerName`) |
| MiR | Payload |
| Locus Robotics | Tote / Cart |
| VDA 5050 | Load |

A bin with no payload code is an **empty**. A bin with a payload code is stock only once its manifest is confirmed;
until then it is neither stock nor an empty.

### Label

A bin's identifier: free text, unique when not blank. The convention is `SHG:NNNN` (e.g. `SHG:0042`): the `SHG:`
prefix tells a Shingo label from other identifiers a plant already puts on a container. Creating bins in a batch
increments a label's trailing number.

The label is an **identifier**, not a verification gate. Nothing in the running system reads it off the physical
bin.

### Bin status

A bin's lifecycle state: `available`, `staged`, `flagged`, `maintenance` or `retired`
([data-model.md](data-model.md#bin-statuses)). Only `available` and `staged` are sourceable. Flag, Maintenance and
Activate on the bin's Actions panel set the status where the bin stands and move nothing; Retire takes the bin out of
operations for good and clears its node, and the record is kept.

### Payload

A template defining what a bin should contain: its parts and quantities and its UOP capacity, the production cycles
a full load supports. Assigning a payload to a bin gives the bin the payload code, populates its manifest from the
template, and sets UOP remaining to capacity.

| System | Term |
|---|---|
| Shingo | **Payload** |
| Warehouse systems | SKU / Item master |

Fields: a **code** (e.g. `SYN-PARTA`), a description, the UOP capacity, the template manifest, and the compatible
bin types (`payload_bin_types`). If the same physical bin frame carries different parts for different processes,
define separate payloads: a payload is the combination of container configuration and contents. When a bin may be
dispatched is [data-model.md](data-model.md#dispatch-eligibility)'s.

UI text calls a payload a **part** (see [Words on the setup screens](#words-on-the-setup-screens)).

### Manifest

The list of items (parts, materials) inside a bin: part number, quantity, optional notes. A **payload manifest** is
the template; the bin's **manifest** is the actual contents as confirmed by the operator.

| System | Term |
|---|---|
| Shingo | **Manifest** |
| Seer RDS | No direct equivalent (goods are tracked by ID only) |
| Warehouse systems | Pick list / Packing list |

### Order

A transport request to move bins between nodes: the canonical noun across the system. Edge orders come from demand
wiring and operator actions; Core receives and dispatches them to the fleet backend. The Edge's list is `/orders`.

| System | Term |
|---|---|
| Shingo | **Order** |
| Seer RDS | Order (block-based or join order) |
| MiR | Mission |
| Locus Robotics | Job |
| 6 River Systems | Task |
| VDA 5050 | Order (same) |

Every order is issued `pending`, goes to `dispatched` when its whole hand is had, or rests in `queued` with a cause
until it is. There is no `completed` status: terminal completion is `confirmed`. The statuses and their transitions
are generated in [data-model.md](data-model.md#order-statuses); what each means is
[order-lifecycle.md](order-lifecycle.md)'s.

### Manual Order

An admin-created one-off order, on both sides. On the Edge, the `/manual-order` page (Move, Retrieve, Complex). On
Core, the **Manual Order** modal on `/orders` (`apiManualOrderSubmit`), whose tabs are Transport (move, retrieve,
retrieve empty, retrieve a specific bin), Staged and Move Robot. The two vocabularies differ because the operations
do.

### Test Order

A developer/QA tool on Core's `/test-orders` page (`shingo-core/www/handlers_test_orders.go`) for exercising order
paths. Not an operator-facing concept; don't use the term in operator UI.

### Cart (of an order)

A bin an order carries, recorded as one `order_bins` row per **lift**: the plan step that picks it up and the plan
step that sets it down. Every order that carries a bin has its rows, kept after the order ends; an order has no
single "bin" column. Where a lift happens is read from the order's plan, never stored on the row. See
[data-model.md](data-model.md#order-carts-order_bins).

### Cart (two-stage unloader)

The operator-facing word for the carrier of a two-stage unloader. HMI copy on the two-stage panels says cart, and so
do the Core station box slots about it (*Carts with an empty bin go to*, *Carts wait at*); code and other admin
surfaces say carrier. Code reference: `operator-render.js` `confirmFullOff`, `confirmSendOn`; `loaders.js`
`stationSlots`.

### Reservation

A soft, revocable hold recording that an order intends to use a resource — a source **bin** or a destination
**slot** — set *before* the bin is claimed. Reservations are what let Core hold an unfulfillable order (`queued`)
and retry as inventory appears, rather than failing the order. Reclaimed when the owning order ends; never
time-based. See [reservations.md](reservations.md).

### Slot

A node a single bin occupies: a storage position (in a lane, a slot with a **depth**, 1 = front and accessible,
higher = deeper and blocked: `nodes.depth`), a staging location, or a line-side position. A **slot reservation** is
the destination-side mirror of a bin reservation: a soft hold on a node, confirmed atomically with the slot claim.

### Acquiring

The act between `pending` and `dispatched`: asking for an order's whole hand (source bins, destination slot, lanes)
and taking it in one transaction, or parking the order in `queued` with the cause on its row. It is never a stored
status. The **acquiring set** — the orders the fulfillment scanner may ask — is `pending` and `queued`
(`protocol.IsAcquiring`); a `queued` order holds its sources as reservations and is asked again each scanner tick,
and is not "stuck" — demand is operator-driven.

### Sourcing

Two meanings, only one live. As an **order status**, `sourcing` is never entered: no transition leads to it, and the
constant stays so old `order_history` rows render. As the **forecast's** word — the Sourcing page, `sourcing_state`,
`StrictSourcing`, the sourcing verdicts and feed — it is current: whether a (process, style) can be supplied.

### Zone

A logical grouping of nodes, typically a physical area of the facility (a floor, a warehouse section, a production
line area).

| System | Term |
|---|---|
| Shingo | **Zone** |
| Seer RDS | Area (scene area) |
| MiR | Zone / Map group |
| VDA 5050 | Zone (same) |

### Station

A Shingo Edge instance's identity, held as three things:

| | What it is | Owner | Mutable? |
|---|---|---|---|
| `station_uid` | the identity Core correlates all history to | minted once, by the edge or by Core | **never** |
| `display_name` | e.g. `SPRINGFIELD / EDGE-2` | operator, edited on Core | freely |
| `Address.Station` | the transport routing selector | derived — its value *is* the `station_uid` | never |

An edge with no uid mints its own on first boot and writes it to its config (`main.ensureIdentity`); Core records a
uid it has never seen as an **unclaimed** station (`registry.Introduce`, `edge_registry.claimed_at` NULL) until a
person names it. A station created on Core first is enrolled there (`POST /api/edges/enroll`, `apiEdgeEnroll`),
which mints its uid. Replacement hardware for an existing station takes that station's uid, so its history stays
attached. The registration message is `EdgeRegister` ([wire-protocol.md](wire-protocol.md)).

"Station" also names two other things: the HMI inside an Edge ([Operator Station](#operator-station)) and, on Core's
Nodes page only, a loader or unloader ([Loader station](#bin-loader--unloader)). Qualify the word wherever two
meanings could meet.

### Operator Station

One operator screen inside an Edge (`domain.Station`, table `operator_stations`, API `/api/operator-stations`, page
`/operator/station/{id}`): it belongs to one process, claims a subset of that process's nodes, and renders the
operator UI for them. Code says **operator station**; UI text says **screen** (*Operator screens* is the Processes
page's tab), which keeps "station" for the Edge and the loader station.

### Process

One production process at an Edge, typically a line or cell that runs styles in sequence (`domain.Process`, table
`processes`). It holds the style pointers (`active_style_id`, and `target_style_id` during a changeover) and the
optional production counter binding. An Edge runs one or more processes; each runs one style at a time.

### Style

A build configuration a process can run — typically a part variant — that decides which NodeClaims are in effect
(`domain.Style`, table `styles`). It belongs to one process. Switching a process's active style is the changeover
workflow.

### NodeClaim

A per-style binding to a Core node (`domain.NodeClaim`, table `style_node_claims`): it declares the payload,
capacity, reorder behaviour, swap mode and staging. The active style's NodeClaims drive material orders. It is the
one claim data type; see [Claim disambiguation](#claim-disambiguation).

### Claim disambiguation

The word "claim" appears in three places. Only one of them is a data type:

1. **NodeClaim** (data type) — per-style binding to a Core node: "style X wants payload Y at node Z."
2. **An operator station claims nodes** (an assignment) — operator station → its nodes
   (`apiSetStationClaimedNodes`): "this HMI is responsible for these physical nodes." No claim table.
3. **An order claims a bin** (`bins.claimed_by`) — the hard hold an order takes for transport, after its
   reservation.

When writing about "claims," qualify which one: "NodeClaim" for the noun; "station node assignment" or "bin claim"
for the others.

### Role (Claim Role)

The direction of material flow at a node under a NodeClaim, `protocol.ClaimRole`: `consume` (full in, empty out) or
`produce` (empty in, full out). There are no other values; a changeover is a change of active style, and each
claim's swap mode decides what happens at its node.

### Swap Mode

The step-list shape of a material handling cycle at a node, a claim's `swap_mode`. Four are configurable —
`sequential`, `single_robot`, `two_robot`, `two_robot_press_index` (`protocol.ConfigurableSwapModes`, generated in
[data-model.md](data-model.md#swap-modes)) — and a claim must carry one. Two more values exist and are never stored
on a claim: `manual_swap`, which marks a forklift-worked loader or unloader node rather than a choreography (ask
`NodeClaim.IsLoaderNode` instead), and `simple`, a runtime descriptor for a delivery move. What each mode does is
[order-lifecycle.md](order-lifecycle.md)'s. UI text names the modes by their chip words (see
[Words on the setup screens](#words-on-the-setup-screens)).

### Process Node

An Edge-side record that says "process X uses Core node Y" (`domain.Node`, table `process_nodes`): its own ID, the
Core node by name (`CoreNodeName`), process-scoped config (owning operator station, sequence, display name), and a
separate runtime row (`domain.RuntimeState`, table `process_node_runtime_states`: the bound bin, its count, the
node's live orders). Many process nodes can reference one Core node (different processes sharing the same physical
slot).

Person/Employee analogy: the Core node is the human (one per body), the process node is the employment record
(many possible per person, each carrying per-employer context).

### Bin Loader / Unloader

A staging point where an operator fills empty bins (a **loader**, role `produce`) or empties full ones (an
**unloader**, role `consume`) to feed or drain the material loop. Core owns it as an aggregate (`bin_loaders`); the
Edge runtime consumes it as a `domain.Loader`. A loader's **layout** is either a **shared window** loader (several
windows presenting one shared payload set and drawing on one budget) or a **dedicated positions** loader (each
position its own node, one payload, one bin). The model, the loader decision and the Edge's askers are
[bin-loader-unloader-architecture.md](bin-loader-unloader-architecture.md)'s.

**Loader station** — the Core Nodes page's word for a bin loader or unloader (one `bin_loaders` row; a two-stage
unloader is one station made of two loaders). UI text says **station** only on that page's box and card
(**+ Station**, **Delete station**); code says loader. Keep it to that page, and qualify it (**loader station**)
anywhere it could meet the other meanings of [Station](#station). Code reference: `shingo-core/store/loaders`
`Loader`; `shingo-core/www/static/pages/loaders.js`.

### Bare bin type

A bin type with `bare_of` set: the marker Core stamps on a CLEAR at a stage-1 window of a two-stage unloader,
derived from the cart's own type (one marker per cart type, created the first time that type comes through,
`EnsureBareMarker`). It means "this cart holds no bin yet". A bare cart is never handed out as an empty
(`EmptyCarrierWhere`), and a bare type is never in a payload's carrier rules or offered in any picker. At stage 2
the one-tap **Send on** posts a blank code and Core stamps the cart back to its `bare_of` type. No operator surface
shows a marker code; the Edge carries only `leaves_bare` (stage 1) and `takes_bare` (stage 2, a produce loader
whose windows the Edge draws as an unloader's; its bare-cart card offers EMPTY ON or PARTIAL ON). UI: a **bare**
badge on the bin types table, read-only. Code reference: `shingo-core/domain/bin_type.go` `BinType.Bare`.

## Robot and Fleet Concepts

### Available

Whether a robot is accepting new orders from the dispatch system. A robot that is not available finishes its current
task but is not assigned new work.

| System | Term | Values |
|---|---|---|
| Shingo | **Available** (bool) | `true` / `false` |
| Seer RDS | Dispatchable | `dispatchable`, `undispatchable_unignore`, `undispatchable_ignore` |
| MiR | State (Ready) | Ready / Paused |
| VDA 5050 | Operating mode | Automatic / Semi-automatic / Manual |

### Connected

Whether the fleet backend can communicate with the robot.

| System | Term |
|---|---|
| Shingo | **Connected** (bool) |
| Seer RDS | `connection_status` (int, 1 = connected) |
| MiR | Status (online/offline) |

### Busy

In use and unable to take other work. The word has two readings, kept apart:

- **The robot's state** — `fleet.RobotStatus.Busy`, the fleet backend's live flag that the robot is executing an
  order or task.

  | System | Term |
  |---|---|
  | Shingo | **Busy** (bool) |
  | Seer RDS | `procBusiness` (bool) |
  | MiR | Mission status (executing) |

- **The busy interval** every fleet figure reads — per-robot busy and utilization, fleet utilization and average
  load, the daily average, the hourly curve and the peak. A robot is busy from the order's first acknowledged or
  in-transit row until it puts the load down (delivered, or the order's failure), and on an open order until now:
  moving, held or faulted, since a faulted robot is still on its order. The wait before a robot takes the order is
  not busy, nor is the wait for the station's confirmation after delivery. A robot on two orders at once is busy
  once, for the union of the two. `TestOverviewPin_OverlapIsCountedOnce` pins it, including that a day's average
  load equals that day's daily average. Run time
  ([Mission detail — stages](ui-style-guide/31-mission-detail-stages.md)) is not busy time: it measures the work, so
  it leaves fault time out.

### Fleet Backend

The vendor-specific robot fleet management system that Shingo communicates with. Shingo's `fleet.Backend`
interface abstracts over vendor differences.

| System | Term |
|---|---|
| Shingo | **Fleet Backend** |
| Seer RDS | RDS (Robot Dispatch System) |
| MiR | MiR Fleet |
| Locus Robotics | LocusServer |
| VDA 5050 | Master control |

## Operations

### Retry Failed

Re-attempt the current failed operation on a robot (`RetryFailed`). Used after the physical issue causing the
failure has been resolved.

### Force Complete

Manually mark the robot's current task as finished, skipping whatever operation was in progress (`ForceComplete`).
Used when material has been moved by hand or the task is stuck.

### Set Availability

Control whether a robot accepts new dispatch orders (`SetAvailability`).

## Material Concepts

### UOP (Unit of Production)

One cycle of the manufacturing process supported by a bin's parts: a bin with UOP capacity 24 holds enough parts
for 24 production cycles. UOP is the atomic quantity a bin, a payload or a lineside pile carries and a cell
consumes; reorder levels and Core's loader thresholds are counted in it. How it is counted is
[uop-architecture.md](uop-architecture.md)'s. UI text writes it **UoP** (see [Units and casing](#units-and-casing));
code identifiers keep `UOP`.

### Totes on Cart

How many totes stand on a tote cart, filled or empty: `bins.totes_on_cart`. **NULL is unknown** (a cart nobody has
declared, or one emptied since); **0 is tops & bottoms** (below), a cart with no totes on it. It is a count, not a
bin type: there is no tops & bottoms type. Every statement that empties a carrier writes it NULL; a person declares
a height with the CLEAR (layers × the cart's own type's totes per layer, 0 declaring tops & bottoms); the admin bin
edit sets it; an admin re-type blanks it; Core derives it at a tote press when the cart outside the press is lifted
(what the press's build drew from it): an outside known at 0 stays 0, and one the count overdrew is written unknown,
never 0. "Bare" is the marker only (a carrier type's bare marker: a cart with no bin on it) and never means tops &
bottoms or a vacant position.

### Totes per Layer

How many totes make one layer on a cart of a bin type: `bin_types.totes_per_layer`, NULL when the type is not a
tote cart. A count of totes is a height only on an exact multiple of it.

### Tote Layers

How many layers of totes a payload's full stack stands: `payloads.tote_layers`, NULL when the part states none. It
is the height an empty is wanted at when it is set down anywhere but the press of a cell fed from behind (there, 0:
tops & bottoms first, else the shortest stack). An empty is ranked by it and never refused for it, but for tops &
bottoms, which a stack is never offered.

### Tops & Bottoms

A tote cart at a known 0 totes with no part on it: the cart's frame alone (`protocol.CarrierTopsAndBottoms`). It is
nobody's ordinary empty: a want of no height, or of a stack, never takes it; a want of 0 (the press of a cell fed
from behind) takes it first; a want of it alone (a kanban's tops & bottoms row) takes nothing else. A kanban keeps it
on a row of its own (`kanban_layer_levels` at 0 layers), a cap beside the type's level that neither takes the
other's room; the keeper never fetches it (it is made at the unloader, and the card asks the crew for it), and a
group that keeps the type with no such row refuses one. On a cell's own position it belongs to that cell, so a
press's index leg lifts it whatever height the press would ask for. An unknown count is not tops & bottoms: it is
unverified.

### Vacant

A position with nothing standing on it (`planVacantLine`). A press's on-deck (its outside or second paired
position) that is vacant when a steady pair reserves has its carrier fetched from the claim's inbound source,
unless another live order is already bringing one there, when the pair waits ("Waiting for outside material"). A
changeover's legs are never fetched for (the changeover brings a vacant position its own carrier), nor is a leg
whose demand episode Core cannot name; they wait. Never "bare", which is the marker: older Edge code and comments
say "bare position" for a vacant one, and the two must not be confused.

### UOP per Tote

The cycles (UOP) that fill one tote of a payload: `payloads.uop_per_tote`, NULL when the part states none. With it,
Core turns a press's count into the totes it filled. Without it, no tote count is derived.

### Supermarket

An automated storage area, represented as a node group (`NGRP`) whose children are lanes and flat storage slots.

### Lane

A linear sequence of storage slots within a supermarket, a synthetic node of type `LANE`. The front slot (depth 1)
is robot-accessible; deeper slots are blocked by those in front. Lane depth varies. Because only the mouth is
reachable, a lane needs arbitration between orders that want it at the same time — see [lanes.md](lanes.md).

### Reshuffle

Moving the bins in front of a buried target out of the way so it can be retrieved: the blockers are parked in free
slots anywhere among the group's children (`findShuffleSlots`) and are never moved back. See
[lanes.md](lanes.md#retrieve--reshuffle).

### Reachable / Buried

A slot is **reachable** iff no occupied slot sits strictly shallower in the same lane; otherwise the bin in it is
**buried**. One definition, `LaneBlockerPredicate`.

### Mouth

The right to work a lane, held as a reservation row (`resource_kind='mouth'`) keyed on the lane. Carries a
**mode**.

### Mode (of a mouth hold)

The work direction: `inbound` (the owner drops into the lane), `outbound` (the owner picks from it), or `dig`
(exclusive — both directions at once). A dig excludes every other owner; anything else shares only on an exact
same-mode match. An order holds one mode per lane.

### Dig

An excavation: moving blockers out of a lane so a buried bin can be reached. The `mode='dig'` mouth row is the
admission claim (one dig per lane); the occupancy row is the corridor mutex. A dig dwells in the lane it is digging
and yields to the dig already there. A `dig` row does not by itself mean an excavation is running — every demand's
source hold is a dig; `reserved_by` tells an excavation from ordinary sourcing.

### Occupancy

A robot is physically inside a lane right now (`resource_kind='occupancy'`). A different fact from holding the claim
on the work.

### Admission

The decision "may this move happen at all", asked in one place (`dispatch/admission.go`). Distinct from **ordering**
— admission says the lane cannot take the move; ordering says it could, but somebody else should go first.
`lane-target-buried` is admission; `lane-deeper-pending` is ordering.

### Gate / Mark

A lane is **gated** iff its node group carries a dwell point (`PropGroupWaitPoints` on the NGRP) — the **mark**. A
per-lane `PropLaneGatePoint` still wins when set. The mark chooses where an order waits: parked pre-dispatch, or
dwelling at a point. It is not a safety setting. See [lanes.md](lanes.md).

**Waiting spot** — a map point beside a group of lanes where a robot stands while Core decides whether it may enter
a lane. A group's waiting spots are its `group_wait_points`. Every lane has one: a lane cannot be created, or moved,
into a group without a waiting spot (`ErrLaneUnmarked`), and a group with lanes cannot lose its last one. UI: the
node modal's *Waiting spots* section is the one editor; a group without any shows *Give this group its waiting spots
to add a lane* in place of *+ Add lane*. Code reference: `shingo-core/store/nodes/marks.go`;
`dispatch.PropGroupWaitPoints`.

### Chapter

One generation of a compound. A superseded generation is a **closed chapter**; `orders.open_for_children` records
sealedness explicitly.

### Headroom

Free slots a group keeps so a dig has somewhere to put blockers. A group filled to the brim cannot excavate at all.
Distinct from the slots one dig needs (a bin at depth *d* has up to *d* − 1 blockers to park), which sizes the
deepest single dig, not the group.

### Queue Cause

The named reason an order is waiting, recorded on its row. Core vocabulary that never crosses the wire. Every cause
declares what releases it (the releaser inventory) and is backed by a periodic floor. See
[queue-releasers.md](queue-releasers.md).

### Floor

A periodic pass that re-evaluates a wait an event should have released. A floor is a **backstop**, not a poll — the
interval is a maximum wait. See [sweeps-and-monitors.md](sweeps-and-monitors.md).

### Staging (declared)

A **staging node** is a spot a robot sets a bin down on its way to or from a line: a station with no parent. Core
cannot recognise one by structure — every station carries the one `STATION` node type, the plant spec's `Kind` is
advisory and never persisted, and the designation lives in the Edge's cell config — so a staging node is never a
storage dropoff to Core's role test (`storageDropoffOf`). The Edge therefore declares every staging dropoff it
builds exclusive (`stagingDropoff`, `ComplexOrderStep.ExclusiveSlot`), and Core reserves a declared dropoff like a
storage slot (`slotNeeds`), so two orders cannot both take one. `TestEveryStagingDropoffIsDeclared` and
`TestDeclaredStagingDropoffIsReserved` hold the two halves. Do not trust a node's name for this: `SLN` (Staging Lane
Node) is a naming convention, and a node can be named `SLN` without being a declared staging node, and vice versa.

### Inbound Staging

The node named by a claim's `inbound_staging`: where a single-robot or two-robot swap parks the incoming bin on its
way to the line, and where a press's tooling changeover holds the incoming style's bins until tooling is done. Its
mirror, `outbound_staging`, is where a single-robot swap parks the departing bin. It means only that. A spare kept
for the line is the **keep-staged node**, a field of its own.

### Keep-Staged Node (the spot)

The node named by a claim's `keep_staged_node`: a spot with one spare kept standing for the line, which a REQUEST's
swap draws from first (from the inbound source when no right spare stands there) and the keep-staged keeper refills
from the inbound source by plain retrieves (`shingo-edge/engine/keep_staged_spot.go`). Naming one turns keep-staged
on, in every swap mode; blank is off.

### Changeover

The workflow for switching a process from one style to another (`process_changeovers`). The changeover **row** has
three states — `active`, `completed`, `cancelled` — and moves once, to one of the two terminal ones
(`domain.ChangeoverState`). The sequencing is per **node task** (`domain.NodeTaskState`), not per changeover:
`swap_required → staging_requested → staged → empty_requested → line_cleared → release_requested → released`, plus
`unchanged`, `switched`, the dispositions `error` / `capacity_blocked` / `awaiting_material` / `abandoned` /
`cancelled`, and `verified`, a terminal state nothing writes. Cutover is gated on all of them being terminal.

### Paired Position

The node named by a claim's `paired_core_node`. The field means two different things depending on the swap mode: at
a **sequential** (A/B) cell it is the partner position the line draws from while this one is swapped; at a
**press-index** cell it is the back press position in a queue. One field, two meanings — read the mode before
reading the field.

### Departed

A swap leg that has finished with its **cell** but is not finished as an order: the fleet has confirmed the last step
of the leg's plan whose node belongs to the cell, and the robot is now carrying a bin away from it.

A departed leg is still live — Core, telemetry, the orders page and swap-peer handling all still own it. What it
stops being is the *cell's*: the two admission guards, the station card's action button and the auto-relief tick all
treat the cell as free. Rendered on the card as **TO MARKET**, the opposite number of ROBOT IN TRANSIT (a cell
*waiting on* a delivery). Derived from `steps_json` against the claim's cell set, never from the swap mode
(`shingo-edge/engine/leg_departure.go`). See [order-lifecycle.md](order-lifecycle.md).

### Other material-flow terms

| Term | Definition |
|------|-----------|
| **Bin Type** | Physical container classification (size, form factor) |
| **Manifest Confirmed** | Whether an operator has verified the bin's contents match the payload template |
| **Depth** | Slot position in its lane (1 = front/accessible, higher = deeper/blocked) |
| **FIFO** | First In, First Out — oldest material is retrieved first |
| **Contiguous Packing** | Occupied slots form an unbroken block from the back of a lane |
| **Reorder Point** | A claim's level on the Edge (`style_node_claims.reorder_point`): at or below it, with AutoReorder on, the level sweep asks for material. Core's loader threshold is a different number ([uop-threshold-replenishment.md](uop-threshold-replenishment.md)) |
| **AutoReorder** | A claim's setting (`auto_reorder`) for whether a breached level orders by itself or waits for the operator's REQUEST |
| **Outbound Destination** | Node where spent or finished bins are sent after removal from the station |
| **Inbound Source** | Node or node group where replacement bins are retrieved from |
| **Payload Catalog** | The payloads available to an Edge, synced from Core (`payload_catalog`) |

## Kanban

A kanban is the card loop of empty carriers around a process: the card is a cart, the pool is the number of cards in
the loop, the level is the mix held ready, the empty is the signal, the sources are where the loop borrows from. It
holds EMPTY carriers; a part's stock at a line is threshold replenishment. Core's keeper is
`shingo-core/engine/kanban_keeper.go`; [dev-env/kanban-rig.md](dev-env/kanban-rig.md) is a worked plant.

### Kanban
A node group that declares a **level** per bin type and a **pool** per **carrier family** (`kanbans`). Core counts
its **loop**, admits every cart of the loop, fetches only what the loop has lost, and caps stores from outside the
loop at the level.

### Carrier Family
The physical cart a bin type is one setting of (`carrier_families`, `bin_types.family_id`). Every type is of one
family, by default its own; a bare marker is of its carrier's. A CLEAR may re-stamp a bin within its family, never
across. **Footprint** is the label and the check, never the key.

### Loop
Where a kanban's carts may be: its slots, its supports' positions, the windows of every unloader whose outbound it
is, and the flights between them.

### Pool
How many carts of a family a kanban's loop is meant to hold (`kanban_families.pool`). Declared; **found** is counted
against it.

### Found
The loop's carts of a family by place: slot, line, window, flight. Below the pool a cart has left the loop; above it
a stranger is in it.

### Room
The kanban's slots whose bin types admit the family. Below the pool a return cannot always land.

### Level
The ready mix of one bin type a kanban holds at all times (`kanban_levels.want`); not the pool. Want is level +
**cover**, with no clamp.

### Cover
The positions the running style's claims put on the line, per bin type. Counted from the claims, never declared. A
process runs its target style while a changeover is open, otherwise its active style.

### Minimum Pool
The sum of a family's levels plus, summed over the supported processes, the most positions any of that process's
styles puts on the line in the family. Below it the line eats the mix.

### Floaters
Pool less mix less the line: the family's carts standing in the kanban as whatever they came off as, re-stamped only
when a type is short.

### Footprint
A bin type's width by length, either way round (`domain.BinType.Footprint`). Members of a family must agree. Not the
backstop **Floor**.

### Supports
The process nodes a kanban serves (`kanban_supports`). A kanban with supports needs a station, the board its
keeper's orders show on.

### Sources
The ordered groups a kanban's keeper may pull from (`kanban_sources`, in rank order); an empty list searches the
plant. A lineside kanban's keeper never pulls from another kanban that has supports, and an unloader repays the
kanban, then the first source short of the cart.

### Strict
Nobody else may pull this kanban's owned carts (`kanbans.strict`). A cart above a kanban's pool (found, flights
included) is owned by nobody: any keeper's deficit ask takes it first, whatever its own sources, and a kanban never
gives a cart it owns.

## UI words

The naming rules the UI follows. Code and wire names keep their own spelling; these govern rendered text.

### Units and casing

UI text writes **UoP** — labels, headers, table columns, prose, toasts, tooltips — never "UOP" or "uop". The rule is
for display text only: code identifiers, JSON keys, struct fields and `data-*` attributes keep their casing
(`UOPRemaining`, `uop_remaining`, `data-uop`), because renaming a serialized field breaks the protocol.

A raw count of `UOP` in the tree means nothing, since most occurrences are identifiers. The question is which
**rendered** text says `UOP`, and a guard must tell a label from a field ref. The discriminator is a word-boundary
match:

```
(?<![A-Za-z0-9_])UOP(?![A-Za-z0-9_])
```

It fires on `>UOP Remaining<` and `' UOP)'`, and stays silent on `{{.UOPRemaining}}`, `data-uop-capacity`,
`remainingUOP` and `MinHysteresisUOP`. The end state is that pattern returning nothing in any `.js` / `.css` /
`.html` under `shingo-core/www`, `shingo-edge/www` or `shared/`. No test enforces it.

### Words on the setup screens

One word per thing on the Edge Processes page and the station composer. A field keeps the name its claim gives it
([Desktop composer patterns](ui-style-guide/16-desktop-composer-patterns-u9.md) › "A field is called what the claim
calls it"); these are the nouns around the fields.

| Say | Never | Why |
|---|---|---|
| **part** | payload (in UI text) | the plant's word on every surface, from the picker to the finding pill. `payload_code` stays the field name in code and on the wire; field labels and refusals say *part* (*Auto request part*; a claim with none is asked for a part) |
| **flow** | — | what one part does on this process: which positions it works, how each swaps, where its bins come from and go |
| **recipe** | preset (in UI text) | a named **shape** of flow — the Processes page's *Recipes* tab. The code calls it a preset (`flow_presets`). A recipe is never a part and never names one; parts are applied to it |
| **screen** | station, for an HMI | see [Operator Station](#operator-station) |
| **1‑Robot Swap**, **2‑Robot Swap**, **2‑Robot Index**, **Sequential A/B** | Single-robot swap, Two-robot swap, 2-Robot Press Index | the chip's word, everywhere, server refusals included. Go's `SwapModeWord` and `composer-model.js`'s `MODES` are the one table (pinned, see [Drift detection](ui-style-guide/24-drift-detection.md)) and keep its non-breaking hyphen |
| **Add a position** | — | one phrase on every door that turns a position on in a flow (the table's *+ Add a position*, the free-position menu, the empty bar's *Add a position below, or use a preset*) |
| a node name, in full | `PLN_01` → `P01` | the plant named its nodes; a shortened name is a second name nobody can search for. Too long for its box, it is cut in the middle with the full name in the tooltip |

The changeover picker's red pill says what Core answered — **No bin at Core** (at least one part has no available
bin), with Core's own note (`missing PANEL-A`) on the row and as the pill's title — and never "No parts". A style
Core has not checked reads **Not checked at Core**. A pill that says more than its source said is a pill that sends
someone looking for the wrong problem.

### State words

What a process or a station is doing is **three words, derived, never stored**:

| Words | When |
|---|---|
| **Changing over** | the process's `target_style_id` is set and differs from its `active_style_id` |
| **Running `<style>`** | otherwise, when a style is running (the station view's `current_style`; the process list's running style) |
| **No part running** | neither |

Changing over is tested first: during a changeover the old style is still the running one, and "Running Style A"
would hide the changeover. Both readers — the station's `stationState` (`operator-render.js`: header, style chip,
footer badge, empty grid) and the Processes list's State column (`processes-desktop.js`) — derive from fields their
payloads already carry, and each surface reads its one derivation so its places cannot disagree.

**Never store a state you can derive.** A stored copy of a fact two other fields already hold can only fall out of
step with them. Before adding a state column or payload field, check whether the words can be computed from what is
already sent; if they can, compute them, in one function per surface.

## Naming Conventions

- **Go structs** use Shingo terms: `domain.Bin`, `domain.Payload`, `domain.Node` (Core), `domain.NodeClaim` (Edge)
- **JSON API fields** use `snake_case`: `bin_type_id`, `payload_code`, `manifest_confirmed`
- **HTML/CSS classes** use `kebab-case`: `tile-loc`, `manual-order-tab-content`
- **Vendor-specific code** (the RDS client `shingo-core/rds` and its fleet adapter `shingo-core/fleet/seerrds`) uses
  the vendor's own terminology
