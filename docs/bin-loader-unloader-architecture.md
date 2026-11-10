# Bin Loader / Unloader

Verified against: fe06474a

This document covers the bin loader and unloader: the forklift-worked staging points where material enters and
leaves the AMR system. Loaders bring material in (an operator fills empties); unloaders take it out (an operator
empties fulls). Core owns their configuration and decides every carrier they are asked to take; the Edge says when
to ask and runs the operator's panel.

---

## Concepts

### The loader aggregate

A loader is one `bin_loaders` row on Core (columns: [data-model.md](data-model.md#core-bin_loaders)):

- `role` is `produce` (a **loader**: the operator fills empties, full bins leave) or `consume` (an **unloader**:
  the operator empties fulls, empty bins leave) — the two values of `protocol.ClaimRole`.
- `layout` is `shared_window` (several windows presenting one shared payload set, `bin_loader_payloads`, and
  drawing on one budget) or `dedicated_positions` (each position its own node and payload). It is the one
  discriminator for how the homes behave.
- `replenishment` is `threshold` (Core's UOP threshold orders its empties,
  [uop-threshold-replenishment.md](uop-threshold-replenishment.md)) or `operator` (see
  [Operator-driven loaders](#operator-driven-loaders)). An unloader cannot be threshold-driven
  (`ErrConsumeThreshold`): consume loaders drain.
- `inbound_source` and `outbound_dest` are where its carriers come from and go; `funnel_windows` says whether a
  shared loader spreads a demand over its windows or funnels it to its first; `auto_push` arms the Edge's
  window-free asks; `second_stage_loader_id` joins the two loaders of a two-stage unloader.
- `no_clear_partials` turns off a dedicated loader's **Clear partials** (see [Clear partials](#clear-partials)).

Its homes are `bin_loader_homes` rows (a window or a dedicated position; `UNIQUE(position_node_id)`, so a node
belongs to at most one loader; `home_kind` for a dedicated home with no part). A loader's carrier mix
(`bin_loader_quotas`, how many of each bin type it wants) and each window's capability set
(`bin_loader_home_bin_types`; a window with none takes any type) stay on Core, where dispatch's finder reads them.
The `min_stock` columns are dormant.

The Edge holds a read-only projection: `node.list_response` carries `Loaders []LoaderInfo` (`BuildLoaderInfos`)
beside the topology, the Edge writes it to its `core_loaders` / `core_loader_positions` / `core_loader_payloads`
cache, and the runtime reads it as `domain.Loader` through the [LoaderStore](#loaderstore-config-resolution). A
process node is a **loader node** when it is a window or position of a Core loader (`LoaderStore.LoaderForNode`) or
its claim says so (`NodeClaim.IsLoaderNode`); `Loader.SynthClaim` builds the in-memory claim such a node runs on
(swap mode `manual_swap`, the loader's loadable parts, its inbound source and outbound destination).

### The four orders around a loader

| Name | What it is | Who makes it |
|---|---|---|
| **L1** | a loader's empty-in: a `retrieve_empty` to a loader window | Core, by the loader decision |
| **L2** | the loaded full's move out to the loader's `outbound_dest` | the Edge, filed by the operator's LOAD (`loadedSendOut`) |
| **U1** | an unloader's full-in: a `retrieve` of a full to an unloader window | Core, by the loader decision |
| **U2** | the cleared empty's move out | the Edge, filed by the operator's CLEAR (`createUnloaderEmptyOut`) |

### A multi-order queue

A loader node takes more than one live order at a time. `CanAcceptOrders` admits a loader node without the serial
check every other process node gets — one active and one staged order, the runtime row's `ActiveOrderID` /
`StagedOrderID` (`domain.RuntimeState`). The changeover gate still applies first. Core's fulfillment scanner works
the queue: it skips an order it cannot source or deliver and dispatches the next, so one part with no empties does
not hold the others.

---

## The operator's cycle

**Loader.** A robot delivers an empty to a window (an L1). The operator LOADs it with a part — the `bin.load` ask
(`Engine.LoadBin`). The Edge refuses a part outside the loader's loadable set (`loadablePayloads`), a load on a
paired or on-deck position (which holds only an empty carrier), and a load on a window whose bin is not empty; the
bin it checked is the bin the request names. Core writes the manifest and the count (the operator's, or the
payload's standard pack when none was declared) and answers; the follow-up (`afterLoad`) confirms the L1, seats
Core's count on the node, and files the L2.

**The load modal needs no request until LOAD.** The station view carries, for each part the board's load modal
can open on (a loader tile's parts, a stage 2's partial and full parts), the part's load template from the Edge's
copy of Core's catalog: its UOP capacity, which the count starts at, and its template lines, which the modal shows
(`load_templates`, `StationService.loadTemplatesForBoard`, held in memory between catalog writes). The modal posts
the part and the count and no manifest; the Edge builds the bin's manifest from the same copy, each template line's
per-cycle ratio times the count (`Engine.loadManifest`), and refuses a payload the copy has no row for. A request
that carries a manifest (the Production page, whose counts a person may edit) is sent as it came. The modal falls
back to `GET /api/payload/{code}/manifest` only for a part the view did not carry.

**Unloader.** A robot delivers a full to a window (a U1, sent with `SkipAutoConfirm` so Core does not confirm it on
arrival). The operator CLEARs it — the `bin.clear` ask (`Engine.ClearBin`) — and Core clears the manifest; the U2
takes the empty out. With no outbound destination the clear still commits, and the operator is told the carrier is
staying.

A loader node's claim always auto-confirms its deliveries (`NodeClaim.IsLoaderNode` forces it; `Loader.SynthClaim`
sets it): a forklift window has nobody at a screen to confirm one. The operator's LOAD or CLEAR is what moves the
bin on.

**Bin identity.** A LOAD writes the bin record standing at the window, the one the Edge checked empty. If the
forklift swapped the physical container, the record still describes what stands at the window: what matters is
the manifest, the count and the location.

### Clear partials

A dedicated loader keeps partly used bins in its buffer spots (`home_kind` `buffer`). When a home holds a stocked
bin of its part and a buffer spot of the same loader holds a stocked bin of that part, the board's Clear partials
section has a card for the home, "Pull the full · partial from" and the spot's name (the Edge stamps the spot on
the home's tile, `bufferPartialFor`). The driver pulls the full and taps; the tap is Core's home clear (the `clear_home` ask,
`Engine.ClearLoaderHome`), which records the full gone and brings the partial home.

`bin_loaders.no_clear_partials` turns this off for one loader: the Clear partials switch on Core's loader settings,
drawn for a dedicated loader with a pinned home and a buffer spot, on by default. Off, no home names a buffer
partial, the board draws no buffer section (strip, words and cards) and no PULL FULL FROM HOME, and Core refuses
the home clear. The buffer's partials stay in the loader's source pool (`Home.InSourcePool`). It rides the wire as
`LoaderInfo.NoClearPartials`; a Core that predates the field sends nothing, which reads as on, so deploy Core
first.

---

## Core: the demand registry and what orders a loader

### Demand registry

Core keeps `demand_registry` (station, core node, role, payload, outbound destination, `replenish_uop_threshold`,
loader), derived from the `bin_loaders` aggregate (`BuildDemandRegistryFromAggregate`). The writers go through
`store.DeriveDemandRegistry` → `demands.SyncDerivedRegistry`, which deletes the station's rows and re-inserts the
derivation in one transaction, refuses an empty derivation that skipped an input it could not resolve while the
station holds rows, and logs one `demand registry derive:` line per station. It runs from the seed, on every loader
config edit (`rederive` in `service/loader_service.go`), and on edge (re)connect (`core_data_service`).

### What orders a loader's carriers

- **Produce (loader L1):** Core's threshold monitor orders a threshold-mode loader's empties directly, and the
  Edge's operator push asks for an operator-driven one's. No bin-count trigger exists.
- **Consume (unloader U1):** the Edge asks Core for a U1 full-in (`offerUnloaderFulls`, one `loader.retrieve` ask
  naming the unloader's parts), and Core decides and creates it. The ask is sent by the unloader's own auto_push
  gates and the startup sweep, and (one payload, through `MaybeCreateUnloaderFullIn`) by a produce release whose
  disposition is `capture_lineside` — a changeover evac at count 0, the sim's auto-operator or an API client; a
  station's HMI posts a bare body on a produce node and never fires it. No bin-arrival trigger exists.

**Deduplication.** The loader decision subtracts what is already coming before it orders, under the loader's lock,
so a payload that already has a live order in flight creates nothing. Core's `UNIQUE(edge_uuid)` constraint
catches any remaining duplicate from at-least-once delivery.

**Startup sweep.** On Edge startup, after registration ack, `SweepPushLoaders` / `SweepPushUnloaders` ask Core for
every auto loader's and unloader's carriers (`loader.retrieve`), picking up demand that arrived while the Edge was
offline; Core decides what, if anything, to order.

### The dropoff gate

Before dispatching, the scanner's dropoff gate (`dispatch.CheckDropoffCapacity`, `plan.DropoffCapacity`) refuses a
node that is not physically free: a bin standing there (the operator has not cleared it yet), or another order
delivering there that holds a claimed bin (`orders.InFlightForDropoffSQL` — a holder, not a status). An order counts
from the moment it claims its bin, and the gate runs again before every re-dispatch, so two orders that were both
parked — both refused by the fleet and demoted, for instance — cannot both go
(`TestFleetRecovery_TwoDemotedOrdersToOneLineOnlyOneGoes`). A read it cannot make blocks.

---

## The loader decision (the never-2N guarantee)

Every carrier a loader or unloader is asked to take — a loader's empty-in (an L1 `retrieve_empty`) or an unloader's full-in (a U1 retrieve of a full bin) — is decided and created on **Core**, by one door, `dispatch.ReplenishLoader` (`shingo-core/dispatch/loader_replenish.go`). Its callers are Core's threshold monitor (`fireSignalCached`) and the door that answers the Edge's `loader.retrieve` ask (`AskLoaderRetrieve` in `shingo-core/www/doors.go`). The invariant: **one demand of N → exactly N bins in flight across the loader's delivery targets, never 2N** — in either direction (`ReplenishRequest.OrderType` states the pull: `retrieve_empty` or `retrieve`).

How it works:

- **A stated origin.** `ReplenishRequest.Origin` is `threshold`, `operator` or `side_cycle`, never defaulted; a request without one is refused. A threshold ask is sized from the reading (`BinsToReachThreshold`) and is refused at an operator-driven loader; a person's ask (`operator`) is sized as asked. Both subtract the demand episode's own live orders. A release's full-in (`side_cycle`) has no episode, so each part subtracts its own live orders across every window of the unloader, and stops for a full of the part already standing on a target window (`orders.ReadPartAtLoader`, one statement).
- **One decision at a time per loader.** What is coming is read, the windows are chosen and the orders are inserted under one per-loader lock (`loaderLock`, `lockorder.LoaderBudget`), so a second ask for the same loader reads the first one's orders. Two different loaders never block each other. The acquires (which may dispatch) run after the lock is let go.
- **The window choice is the planner's.** `plan.LoaderWindows` walks the loader's delivery targets (`loaders.DeliveryTargets`: a spreading shared loader gives every window and a budget of the window count; a funnelling shared loader and a dedicated position give one node and a budget of 1) and orders one carrier per window it chooses. It holds a window, and names why in `HeldBy`, when the capacity verdict refuses it (a carrier standing, or an order holding a bin on its way), when another order's slot reservation speaks for it (`window-reserved`), when a carrier of the ask's own kind is already asked for there by an order holding no row on it yet, an Edge order before its acquire (`window-order-open`), or when a read fails (`window-check-failed`). A live Core ask holds its window's slot row from its issue (`Need.Window`), so another ask at that window, of either kind, is held `window-reserved`, and an ask whose window was taken since the choice read it is no order: the loop asks at its next window.
- **Multi-window delivery is per loader.** The Core `bin_loaders.funnel_windows` field alone decides it. Spreading, a demand of N at an N-window loader orders exactly N, one per free window, never two at the same window; funnelling, the loader takes its first window only, budget 1. An Edge config that still carries the retired `loaders_multi_window` key loads and ignores it.
- **A release's full-in names its parts.** The side-cycle ask carries an ordered list (`LoaderRetrieveRequest.Payloads`), decided in that order in one decision under the lock (`replenishPartsLocked`): each part sees the windows the parts before it took, and the answer carries one outcome per part (`LoaderRetrieveResponse.Parts`: its order, or `Skipped`, or `HeldBy`). A person's ask names its one part in `PayloadCode`.
- **Fails closed.** Every read that cannot be made orders nothing; the next ask or reading retries.

An order a claim sends straight to a loader window (a changeover's retrieve, a cell's move) is still created on the Edge. Core holds the window against the asks once that order is there: the capacity verdict's in-flight arm sees it once it holds its bin, and before then only the open-ask count of its own kind does.

**What the Edge does.** The Edge says *when* to ask, from triggers only it sees, and decides nothing about a loader's windows. It sends `loader.retrieve` (`protocol/ask.go`: `LoaderRetrieveRequest`, `LoaderRetrieveResponse`) with an origin, and the orders Core makes reach it as projections. It holds no loader lock and no transaction around them, because it creates none of them. The askers (`shingo-edge/engine/loader_retrieve_ask.go`):

- **Loader side:** `RequestEmptyBin` at a loader window, and the window-free push (`maybeStageLoaderEmpty` → `stageOperatorEmpty`, from `rePushOwnLoader` on a produce CLEAR or an L2's landing, and `SweepPushLoaders` at startup), both through `askLoaderEmpties`, origin `operator`.
- **A person's full:** `RequestFullBin` at a consume window, origin `operator`.
- **The manual retrieve:** `CreateRetrieveForAPI` forwards a retrieve to a node a loader or unloader owns (`loaderOwning`) as a person's ask; when Core makes nothing, the caller gets Core's sentence as a 409 (`LoaderRetrieveRefused`). A destination no loader owns is created directly (`createRetrieveDirect`).
- **The unloader's U1:** `offerUnloaderFulls` sends one ask per unloader, naming the parts it serves in order, origin `side_cycle`, and does not wait; until Core answers, the unloader's first window on this Edge carries a pending note on the board. Its triggers are a produce release carrying `capture_lineside` (`MaybeCreateUnloaderFullIn`, from the release commit), the auto_push re-pull of the node's own unloader (`rePushOwnUnloader` / `rePushOwnUnloaderIfUncovered`: CLEAR, PUSH EMPTY, the U2's pickup, and the U2's landing as a fallback), and the startup sweep (`SweepPushUnloaders`). An unloader with no inbound source, or with no window on this Edge (`ownWindow`), is not asked for.

**Local pre-checks.** Three checks skip a U1 ask when the Edge's order table already shows Core would order nothing: `unloaderPullsCovered` (one SQLite read), its use at CLEAR, PUSH EMPTY and the U2's landing (`rePushOwnUnloaderIfUncovered`), and the release's local-first check in `MaybeCreateUnloaderFullIn`. They are exact in one direction only: covered means Core's answer orders nothing, because the Edge's rows are a subset of Core's; uncovered (or an unreadable order list) means Core decides. The U2's pickup asks without one: the window is free there, and only Core can see it.

---

## LoaderStore (config resolution)

"Which loader serves this payload / contains this node" is resolved through one consumer-defined interface,
`engine.LoaderStore` (defined in the engine package, not next to the store, so the engine does not import store
internals): `LoaderForPayload`, `LoaderAt` and `Loaders` by role, and `LoaderForNode` for any role, since a node
belongs to at most one loader.

One implementation, `aggregateLoaderStore`, projects the Core-owned cache into validated `*domain.Loader`s (via the
domain constructors) and holds them as an **immutable in-memory snapshot**, swapped atomically (`atomic.Pointer`)
on each node-list sync (`SetCoreLoaders` → `Refresh`). Resolution reads the snapshot, never the DB, so a torn
multi-statement read of the cache is impossible and a DB flicker during a sync only keeps the last-known-good
snapshot.

**Error contract (fail closed).** Every lookup returns `(*domain.Loader, error)`:

| Result | Meaning | Caller |
|---|---|---|
| `(loader, nil)` | resolved | use it |
| `(nil, ErrLoaderNotFound)` | a clean miss | may take its fallback (e.g. payload-first-match) |
| `(nil, other error)` | a real failure (DB read, malformed config) | **fail closed** — must NOT fall open, or a flicker reroutes demand to the wrong loader |

Callers branch with `errors.Is(err, ErrLoaderNotFound)`.

**Consumed by the askers.** The Edge's loader and unloader paths resolve a `*domain.Loader` through the store to
know which loader to ask Core about — `MaybeCreateUnloaderFullIn`, `rePushOwnUnloader` and `SweepPushUnloaders`
resolve a consume `*domain.Loader` and send its `loader.retrieve` ask (`offerUnloaderFulls`). The decision itself
reads Core's own loader row, not this copy.

---

## Protocol

- **Loader config** is Core-owned and rides the node-list sync: there is no dedicated loader-config message.
  `node.list_response` carries `Loaders []LoaderInfo` alongside the topology, and the Edge swaps it into its cache
  and its `aggregateLoaderStore` snapshot atomically with the node list.
- **The asks** are `loader.retrieve` (carriers for a loader or unloader), `bin.load` (LOAD) and `bin.clear`
  (CLEAR), in `protocol/ask.go`.
- **Orders** are ordinary `OrderRequest`s; `RetrieveEmpty` says whether a retrieve wants an empty carrier.

---

## Edge cases

**Operator has no stock for the requested payload:** load a different payload the loader serves; the order for the
skipped one stays queued.

**Core restart:** the fulfillment scanner runs on startup and picks up every queued order.

**Edge restart:** the auto-push sweeps run after registration ack, asking Core for every auto loader's and
unloader's carriers to catch demand that arrived while the Edge was offline.

**Concurrent asks:** Core's per-loader lock in `ReplenishLoader` serializes the read and the insert, so concurrent
asks for one loader order once. Core's `UNIQUE(edge_uuid)` catches any remaining duplicate.

**No bins ever available:** the order stays queued. The operator sees it waiting and can cancel.

## Operator-driven loaders

A loader whose `replenishment` is `operator` is paced by its operator rather than by a UOP threshold: Core's
threshold monitor never orders for it, and its empties come from the window-free push instead — `rePushOwnLoader`
stages one **payload-agnostic** empty (a carrier with no part; the operator binds the part at LOAD) when a window of
that loader frees, on a CLEAR at the window, on an L2's landing, and from the startup sweep (`SweepPushLoaders`),
each one a `loader.retrieve` ask Core counts like any other. Switching the field to `threshold` (with thresholds set)
hands the loader back to the UOP C-push; the operator's REQUEST EMPTY stays available either way.
