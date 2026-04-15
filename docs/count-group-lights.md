# Count-group warning lights

When a mobile robot enters an **advanced zone** in RDSCore (a pedestrian
crossing, forklift aisle, etc.), shingo turns on a PLC-driven warning
light so people in the area are alerted. When the last robot leaves,
the light goes off.

## Data flow

```
RDSCore /robotsInCountGroup  →  shingo-core  →  Kafka (outbox)  →  shingo-edge
                                                                         ↓
                                                          WarLink → PLC request tag
                                                                         ↓
                                                                  ladder logic
                                                                         ↓
                                                                       light
```

Core polls RDS every 500 ms per configured group. After an N-of-M
debounce (2-of-3 ON, 3-of-3 OFF — asymmetric toward staying on), a
transition emits an event, which is shipped to all edges via the
existing outbox → Kafka path. Each edge checks its own bindings map;
if it owns the group, it runs a request/ack handshake against a
dedicated PLC tag via WarLink. The ladder logic drives the light and
clears the request tag to acknowledge.

## Deadman / fail-safe

Two independent fail-safe paths:

1. **RDS-down fail-safe (core).** If `/robotsInCountGroup` fails
   continuously for `fail_safe_timeout` (default 5 s), core issues a
   forced "on" command for the group. The fail-safe releases
   automatically when RDS recovers.
2. **Heartbeat deadman (PLC).** Edge writes a monotonically-
   incrementing counter to a shared heartbeat tag every 1 s. Ladder
   logic drives **all** configured zone lights ON if the counter stops
   changing for >3 s. This covers edge process crashes and network
   partitions between edge and WarLink/PLC.

The heartbeat is intentionally **suppressed at edge startup** until
the Kafka subscription is confirmed. This means a brief window on
every edge restart where the PLC deadman trips lights ON — expected
behavior, not a bug, and ops should not interpret it as a PLC fault.

## Core config (`shingocore.yaml`)

```yaml
count_groups:
  poll_interval: 500ms
  rds_timeout: 400ms
  on_threshold: 2
  off_threshold: 3
  fail_safe_timeout: 5s
  never_occupied_warn: 5m
  never_occupied_error: 30m
  groups:
    - name: Crosswalk1                # Exact Roboshop advanced-group name (case-sensitive).
      enabled: true                   # Edge resolves the binding by this name.
    - name: ForkliftAisleN
      enabled: true
```

## Edge config (`shingoedge.yaml`)

```yaml
count_groups:
  heartbeat_interval: 1s
  heartbeat_tag: Shingo_Alive
  heartbeat_plc: line3_plc
  ack_warn: 2s
  ack_dead: 10s
  codes:
    on: 1                             # DINT value written to request tag for "light on"
    off: 2                            # "light off"
  bindings:
    crosswalk1:
      plc: line3_plc
      request_tag: Crosswalk1_REQ
    aisle_n:
      plc: line3_plc
      request_tag: AisleN_REQ
```

## PLC ladder responsibilities (Nate)

For each zone:
1. On startup, initialize `<zone>_REQ` to `0`.
2. Watch for non-zero values on `<zone>_REQ`.
3. On detecting a non-zero value, take the mapped action:
   - `1` → drive zone light ON
   - `2` → drive zone light OFF
   - (future: bitmask with `3`=flash, `4`=audible alarm, etc.)
4. Immediately after taking action, write `0` back to `<zone>_REQ` to ack.

For the heartbeat deadman:
1. Watch `Shingo_Alive`.
2. If the value hasn't changed in >3 s, drive all configured zone
   lights ON until heartbeat resumes.

Each zone request tag plus the heartbeat tag must have `writable: true`
in the WarLink config — otherwise every write returns HTTP 403.

## Roboshop zone definition (plant ops)

Advanced groups are topological — selections of LM points, AP points,
and route segments. For crosswalk warning that covers both the
crosswalk itself and the robot's approach, **include the approach
route segments** in the group, not just the LM/AP points at the
boundary. A robot traversing an approach route counts as "in the
zone" only if the route is part of the group.

The group name in Roboshop must exactly match the `name` in
`shingocore.yaml` (case-sensitive). A typo produces a permanently-off
light with no runtime error from RDS. The `never_occupied_warn`/
`never_occupied_error` thresholds surface this as a log WARN/ERROR
after the configured duration.

## Observability

- Every state transition on core appends a row to the
  `audit` table with `entity_type = "countgroup"` and includes the
  correlation ID.
- Every edge ack (success, timeout, WarLink error) appends a row
  to core's audit table with `entity_type = "countgroup_ack"` and the
  same correlation ID.
- Together these give forensic visibility: core saw X, edge wrote Y,
  PLC took Z ms to ack (or timed out).

## v1 constraints

- Single PLC per advanced group; single shared heartbeat tag.
- No hot-reload of `count_groups` config — restart on change.
- Polling only — no WebSocket/SSE alternative even if AIVISION offers
  one later.
- Two action codes (`1` on, `2` off). DINT reserves bitmask room for
  v2 (flash, alarm).
- No third-party robot tracking — operators must use the Traffic
  Control Area UI for that (RDS Manual §7.12.3).
