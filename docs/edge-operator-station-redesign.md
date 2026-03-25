# Edge Operator Station Redesign Specification

This document defines a clean-slate architecture for operator stations in Shingo Edge.

Backwards compatibility is not a goal. The purpose of this design is to replace the current operator screen and line-wide changeover model with a system that:

- keeps authoritative ownership in Shingo Edge
- supports optional operator stations
- supports station-specific material responsibility by job style
- supports rolling changeovers across a long process
- allows every operator-station function to be executed from the main Edge computer if a station is unavailable

---

## Design Goals

1. Edge owns all process, material, order, and changeover state.
2. Operator stations are delegated control surfaces, not owners of workflow state.
3. Processes may have zero operator stations and remain fully operable from the main Edge UI.
4. A process may have many operator stations, each responsible for a subset of material positions.
5. Material responsibility is style-aware at the station-position level.
6. Changeover is process-owned but may be executed in rolling fashion by station or by position.
7. The station UI must be touch-first and usable on a 10-inch Raspberry Pi-driven fullscreen display.
8. No station action may require a keyboard or mouse.
9. Every station action must be available from the main Edge computer.
10. Server-side authorization and validation must define what a station can operate. The client UI is not trusted as an authority boundary.

---

## Non-Goals

- No support for the legacy saved-screen designer model.
- No freeform canvas layout authoring as a first-class architecture feature.
- No assumption that the whole process flips job style at one instant.
- No duplication of authoritative workflow state in station-local storage.

---

## Core Principles

### 1. Edge-First Ownership

Shingo Edge is the single source of truth for:

- process configuration
- operator-station configuration
- material position definitions
- style assignments
- live material state
- transport/order state
- manifest confirmation state
- changeover orchestration state

Operator stations never own these records. They only render a derived view and submit intent.

### 2. Stable Physical Hierarchy

The model must separate stable physical structure from style-specific configuration.

Physical structure:

- process
- operator station
- station position
- nodes associated with those positions

Style-specific configuration:

- what material belongs at a position for a given style
- reorder thresholds
- preferred cycle mode
- manifest rules
- changeover behavior

Runtime state:

- what is loaded now
- what is staged next
- what order is active
- what effective style a station position is currently running

### 3. Delegated Execution

A station operates only the material positions assigned to it. The same actions are also available from the main Edge UI. If a station fails, the main Edge computer can take over with no loss of authority or functionality.

### 4. Rolling Changeover

Changeover is not modeled as a single line-wide style flip. Edge calls a process changeover session, but each station or position may execute its material transition at different times.

This supports scenarios like OP10 changing 20 minutes before OP100.

---

## Domain Model

## Entity Overview

```text
Process
  -> OperatorStation (optional)
       -> StationPosition
            -> PositionNodeBinding
            -> StylePositionAssignment
            -> RuntimeMaterialState

Process
  -> ChangeoverSession
       -> ChangeoverStationTask
            -> ChangeoverPositionTask
```

## Core Entities

### Process

An assembled production process managed by one Edge installation.

Fields:

- `id`
- `code`
- `name`
- `description`
- `mode` (`normal`, `changeover`, `paused`, `maintenance`)
- `current_style_id` nullable
- `target_style_id` nullable
- `changeover_session_id` nullable
- `enabled`

Rules:

- A process may have zero or more operator stations.
- A process may run entirely from the main Edge UI.
- A process owns the official declaration of current style and target style.

### OperatorStation

A physical HMI or tablet mounted at a point in the process.

Fields:

- `id`
- `process_id`
- `code`
- `name`
- `sequence`
- `area_label`
- `device_mode` (`fixed_hmi`, `portable_tablet`, `main_edge_virtual`)
- `enabled`
- `expected_client_type` (`touch_hmi`)
- `last_seen_at`
- `health_status` (`online`, `offline`, `disabled`, `faulted`)

Rules:

- Operator stations are optional.
- The main Edge computer is not modeled as an operator station; it remains a privileged global control surface.
- A station can be disabled without affecting process authority.

### StationPosition

A stable physical material responsibility point within a station.

Examples:

- left feeder
- right feeder
- empty bin drop
- partial return point
- label stock
- fastener rack

Fields:

- `id`
- `station_id`
- `code`
- `name`
- `sequence`
- `position_type` (`consume`, `produce`, `empty_return`, `partial_return`, `wip`, `other`)
- `allows_reorder`
- `allows_empty_release`
- `allows_partial_release`
- `allows_manifest_confirm`
- `allows_station_change`
- `enabled`

Rules:

- A position is physical and stable across job styles.
- A position does not directly belong to one style.

### PositionNodeBinding

Maps a station position to the actual process nodes and logistics nodes used by orders.

Fields:

- `id`
- `station_position_id`
- `delivery_node`
- `staging_node`
- `secondary_staging_node` nullable
- `inbound_source_node` nullable
- `outgoing_node` nullable
- `node_group` nullable
- `secondary_node_group` nullable
- `outgoing_node_group` nullable

Rules:

- Node bindings are physical/logistical wiring.
- They are not duplicated per style unless the physical routing truly changes by style.

### JobStyle

The style or recipe the process can run.

Fields:

- `id`
- `process_id`
- `code`
- `name`
- `description`
- `active`

Rules:

- Styles belong to a process.
- Cross-process style reuse is out of scope for this redesign.

### StylePositionAssignment

The style-aware material definition for a station position.

This is the core missing concept in the current implementation.

Fields:

- `id`
- `style_id`
- `station_position_id`
- `payload_code`
- `payload_description`
- `role` (`consume`, `produce`)
- `uop_capacity`
- `reorder_point`
- `auto_reorder_enabled`
- `cycle_mode` (`simple`, `sequential`, `single_robot`, `two_robot`)
- `retrieve_empty`
- `requires_manifest_confirmation`
- `allows_partial_return`
- `changeover_group`
- `changeover_sequence`
- `changeover_policy` (`manual_station_change`, `auto_stage_then_manual_swap`, `central_only`)

Rules:

- A station position may have zero or one assignment for a style.
- Missing assignment means the position is unused for that style.
- Assignments define style-specific material responsibility without changing the physical hierarchy.

### RuntimeMaterialState

Live state for a station position under current operations.

Fields:

- `id`
- `station_position_id`
- `effective_style_id`
- `assigned_style_position_id`
- `loaded_payload_code`
- `material_status` (`empty`, `active`, `replenishing`, `staged_next`, `blocked`, `faulted`)
- `remaining_uop`
- `current_manifest_status` (`unknown`, `pending_confirmation`, `confirmed`, `rejected`)
- `active_order_id` nullable
- `staged_order_id` nullable
- `loaded_bin_label` nullable
- `loaded_at`
- `updated_at`

Rules:

- Runtime state belongs to the physical station position.
- `effective_style_id` may differ across positions during a rolling changeover.
- The process may be globally changing over while some positions still operate on the old style.

### ProcessChangeoverSession

The authoritative process-level declaration that a process is moving from one style to another.

Fields:

- `id`
- `process_id`
- `from_style_id`
- `to_style_id`
- `state` (`planned`, `staging`, `active`, `paused`, `completing`, `completed`, `cancelled`, `faulted`)
- `called_by`
- `started_at`
- `completed_at` nullable
- `notes`

Rules:

- There is at most one active changeover session per process.
- This session does not imply immediate station-wide or process-wide material swap completion.

### ChangeoverStationTask

A derived station-level work package for a process changeover.

Fields:

- `id`
- `changeover_session_id`
- `station_id`
- `state` (`not_needed`, `waiting`, `staging`, `ready_to_switch`, `switching`, `switched`, `verified`, `blocked`, `cancelled`)
- `transition_mode` (`rolling_local`, `central_override`, `auto_only`)
- `ready_for_local_change`
- `switched_at` nullable
- `verified_at` nullable
- `blocked_reason` nullable

Rules:

- A station task exists for each affected station.
- A station may be switched by the local HMI or by the main Edge computer.

### ChangeoverPositionTask

The lowest-level changeover tracking unit.

Fields:

- `id`
- `changeover_station_task_id`
- `station_position_id`
- `from_assignment_id` nullable
- `to_assignment_id` nullable
- `state` (`unchanged`, `stage_next`, `ready`, `swap_required`, `switched`, `verified`, `blocked`)
- `old_material_release_required`
- `next_material_order_id` nullable
- `old_material_release_order_id` nullable
- `effective_style_after_switch`

Rules:

- This entity allows rolling changeover to occur at material-position granularity.
- The process is complete only when all required position tasks are complete or explicitly skipped.

---

## Authority Model

## Main Edge Computer

The main Edge UI can:

- view all stations and positions
- create and manage orders for any position
- confirm manifests for any position
- release empty or partial material for any position
- initiate and orchestrate process changeover
- force, defer, or complete station change tasks
- fully operate a process with zero stations
- fully operate a process when a station is offline or damaged

## Operator Station

An operator station can:

- view only its assigned station positions
- request material only for its assigned positions
- release empties only for its assigned positions
- release partials only for its assigned positions
- confirm manifests only for its assigned positions
- execute local station material change only when permitted by Edge

An operator station cannot:

- redefine assignments
- alter process-wide style declarations
- operate positions not bound to the station
- bypass server validation

---

## Changeover Model

## Problem Statement

The process may require a rolling material transition. Early stations may need next-style material long before later stations.

Therefore:

- the process changeover must be centrally declared
- stations must be able to transition locally at different times
- positions must carry their own effective style during the rolling window

## Process-Level Changeover Lifecycle

1. Edge starts a `ProcessChangeoverSession` from style A to style B.
2. Edge computes affected stations and positions by diffing `StylePositionAssignment` records for A and B.
3. Edge creates `ChangeoverStationTask` and `ChangeoverPositionTask` records.
4. Edge begins staging next-style material where appropriate.
5. Stations become locally eligible to switch as production timing permits.
6. The local station or main Edge UI performs the switch.
7. Edge updates runtime state so the affected positions now have `effective_style_id = B`.
8. The process remains in changeover until all required station-position tasks are complete.
9. When all tasks complete, Edge marks `current_style_id = B`, clears `target_style_id`, and closes the session.

## Station-Level Changeover Semantics

Each station has three concurrent concepts:

- `current station effective state`
- `next-style material readiness`
- `permission to switch now`

This allows:

- OP10 to switch now
- OP20 to keep running style A
- OP100 to remain untouched for 20 more minutes

All while one process-level changeover session remains active.

## Position-Level Effective Style

During changeover, `effective_style_id` is a runtime field on each station position.

This is the field that controls:

- which assignment is active for reorder logic
- what payload the station UI shows as current
- what auto-reorder thresholds apply
- what the main Edge UI must display as actual process condition

The process may be in a mixed-style transition window. This is valid and expected.

## Station Change Actions

Station-level material change may include:

- request next-style material early
- release current empty
- release current partial
- confirm next-style manifest
- execute local swap from old assignment to new assignment
- mark station or position verified

These are station-scoped manifestations of one process-owned changeover session.

## Central Override

If a station is down, disabled, offline, or physically damaged, the main Edge computer can:

- request next-style material for that station
- release old material for that station
- confirm manifests
- mark the station change complete
- continue or finish the process changeover

This is a hard requirement and must be supported by all APIs and UI workflows.

---

## Runtime Behavior

## Material Request Logic

Normal reorder logic operates against `RuntimeMaterialState.assigned_style_position_id`.

During normal running:

- reorder evaluates the assignment for the position's current effective style

During rolling changeover:

- positions not yet switched still reorder against the old style assignment
- positions already switched reorder against the new style assignment
- staged-next material may exist before the switch occurs

## Empty and Partial Release Logic

Empty and partial release must be position-aware.

When a release is initiated:

1. Edge validates that the position allows that release type.
2. Edge validates that the initiating surface is allowed to operate the position.
3. Edge creates the proper transport/order workflow.
4. Edge updates runtime state and changeover task state if this release is part of changeover.

## Manifest Confirmation

Manifest confirmation belongs to the material/position workflow, not to the screen.

Stations may initiate confirmation for positions they own. The main Edge UI may confirm for any position.

Manifest state must be visible in:

- station UI
- central Edge UI
- changeover orchestration views

---

## User Interface Specification

## General HMI Requirements

Operator stations run on Raspberry Pi devices in fullscreen browser mode.

Requirements:

- touch-first
- no keyboard required
- no mouse hover dependency
- large tap targets
- readable at 10-inch screen size
- resilient to intermittent network conditions
- safe to use with gloves if possible

## Station UI Architecture

The preferred station UI architecture is a constrained application shell, not a freeform screen designer.

Recommended implementation:

- DOM-first or DOM-plus-canvas hybrid
- canvas only for process/position visualization if needed
- typed UI components for actions and dialogs
- no general-purpose visual designer

## Station UI Layout

Recommended top-level station views:

### 1. Run

Shows current active material positions for the station.

Each position card includes:

- position name
- current material
- remaining state
- current order state
- primary action
- local alerts

Primary actions may include:

- `Request Material`
- `Release Empty`
- `Release Partial`
- `Confirm Manifest`

### 2. Changeover

Visible only when the process has an active changeover session affecting this station.

Shows:

- current style
- target style
- positions still on old style
- positions staged for new style
- positions ready to switch
- station-level `Switch This Station` or per-position switch actions

This view must support rolling changeover without implying that every station changes immediately.

### 3. Issues

Shows:

- blocked orders
- manifest exceptions
- offline/faulted state
- operator prompts requiring acknowledgement

## Numeric Touch Input

All quantity entry must use on-screen numeric keypad dialogs.

Use cases:

- partial quantity
- final count
- manifest quantity confirmation
- supervisor override values if needed

The keypad must:

- be fullscreen or modal
- support large numeric buttons
- support backspace and clear
- require explicit confirm

## Navigation Rules

- Avoid deep navigation.
- Common actions must be available in one tap from the main station view.
- No hidden tabs for critical safety actions.
- No tiny icon-only actions for frequent workflows.

## Central Edge UI

The main Edge computer must expose:

- process-wide live material overview
- station overview
- station-position detail
- process changeover dashboard
- station-task dashboard
- per-position manual control

The central UI is the operational fallback for every station workflow.

---

## API Specification

## Principles

- APIs are intent-based, not raw screen-driven mutations.
- The client sends station-scoped or central-scoped intent.
- Edge validates permissions against station-position bindings.

## Required Read Models

### Process Overview

Returns:

- process identity
- current style
- target style
- overall mode
- stations and health
- active changeover summary

### Station View Model

Returns:

- station identity
- process identity
- current process mode
- current style
- target style if changing over
- visible positions
- runtime material state for each position
- active orders for each position
- allowed actions for each position
- station-level changeover task state

### Changeover Dashboard View

Returns:

- process session
- station tasks
- position tasks
- staging readiness
- blocked tasks

## Required Command APIs

### Process Commands

- `start_process_changeover(process_id, to_style_id)`
- `pause_process_changeover(process_id)`
- `cancel_process_changeover(process_id)`
- `complete_process_changeover(process_id)` when all requirements are met

### Station Commands

- `request_position_material(station_id, position_id)`
- `release_position_empty(station_id, position_id)`
- `release_position_partial(station_id, position_id, qty)`
- `confirm_position_manifest(station_id, position_id, manifest_payload)`
- `switch_station_position_to_next(station_id, position_id)`
- `mark_station_verified(station_id)`

### Central Commands

The main Edge UI must support equivalent commands without station dependence:

- `central_request_position_material(process_id, position_id)`
- `central_release_position_empty(process_id, position_id)`
- `central_release_position_partial(process_id, position_id, qty)`
- `central_confirm_position_manifest(process_id, position_id, manifest_payload)`
- `central_switch_station_position_to_next(process_id, position_id)`
- `central_switch_entire_station(process_id, station_id)`

## Validation Rules

For a station-issued command:

1. Station must be enabled and recognized.
2. Position must belong to that station.
3. Action must be allowed for that position.
4. If in changeover, the requested action must be valid for the current changeover state.
5. Edge must reject any attempt to operate an unbound position.

---

## Event Model

The station UI and central UI require realtime updates.

Required event categories:

- process mode changed
- station health changed
- position runtime material changed
- order state changed
- manifest state changed
- changeover session changed
- changeover station task changed
- changeover position task changed

Events must be keyed by process and station so clients can subscribe to the minimum necessary scope.

---

## Failure and Degraded Operation

## Station Offline

If a station is offline:

- process continues
- central UI remains authoritative
- orders and changeover tasks continue to exist in Edge
- local station commands are unavailable only at that device

## Station Disabled or Damaged

If a station is disabled or physically damaged:

- the station is marked `disabled` or `faulted`
- its positions remain fully operable from the main Edge computer
- process changeover must not depend on station device health

## Network Loss Between Station and Edge

The station UI must:

- show an offline state immediately
- stop presenting unsafe mutating actions while disconnected
- recover by reloading the server view model after reconnect

No station-side local queue is authoritative.

## Main Edge Computer Override

Central override is always available. It is not an exception workflow. It is a first-class operational mode.

---

## Replacement of Current Architecture

The following current concepts should be removed or replaced:

### Remove

- freeform saved `operator_screens` as the primary station model
- drag-and-drop operator designer as a required workflow
- style-bound material-slot records as the sole representation of physical material points
- single process-wide instantaneous style flip assumption

### Replace With

- process -> station -> position hierarchy
- style-position assignments
- runtime material state by position
- process-owned rolling changeover session
- station-task and position-task changeover tracking
- typed touch UI flows

---

## Implementation Guidance

## Recommended Build Order

1. Define new schema and store models.
2. Build process/station/position configuration UI in the main Edge app.
3. Build style-position assignment UI.
4. Build runtime read models and APIs.
5. Build central Edge material operations against the new model.
6. Build process-level rolling changeover orchestration.
7. Build station UI as a constrained touch application.
8. Add station health/registration and realtime subscriptions.
9. Remove legacy operator designer and saved-screen workflow.

## Migration Strategy

Because backwards compatibility is not required:

- create a new schema and new APIs
- do not preserve old operator-screen assumptions
- port only verified business rules, not UI structure

---

## Summary

This redesign makes five major changes:

1. Operator stations become optional delegated endpoints under an Edge-owned process.
2. Material responsibility is modeled at stable station positions with style-specific assignments.
3. Runtime material state belongs to positions, allowing mixed-style rolling transitions.
4. Changeover becomes a process-owned orchestration with station and position execution tasks.
5. Every operator-station action remains fully executable from the main Edge computer.

This architecture is intended to support both extremes cleanly:

- a small process with no operator stations at all
- a long process with many stations executing a rolling changeover over an extended period
