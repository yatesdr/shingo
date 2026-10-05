## Pending removal

### `swap_mode = "simple"` — RETIRED as a configurable mode (descriptor only)
- **Hidden in UI:** 2026-04
- **Retired as configurable:** 2026-07 (ingress lockdown)
- **Status:** "simple" is no longer a configurable claim mode. `UpsertClaim`
  and `plantspec.Validate` reject it, and the store no longer normalizes a
  blank swap_mode to it (blank now fails loud). It survives ONLY as the runtime
  `protocol.SwapModeSimple` CycleMode descriptor — the node-empty downgrade tag
  and the bare-move result tag (see consume_plan.go / operator_stations.go). A
  hidden `<option value="simple">` remains in the dropdown solely so an existing
  legacy row still renders when opened in edit mode. The allowlist, the
  dropdown, and its drift test all key on `protocol.ConfigurableSwapModes()`.

### `claim.keep_staged` column — REPLACED by `claim.keep_staged_node`
- **Withheld:** 2026-09-10 to 2026-10-03.
- **Replaced:** 2026-10-04. The keep-staged node is named on the claim
  (`keep_staged_node`, blank is off) instead of a flag that armed a spot on
  `inbound_staging`. The claims table's rebuild drops the old column; nothing
  is carried into the name (no plant ever ran keep-staged).
- **Configurable:** flowspec marks it Used for all four swap modes and
  Forbidden elsewhere; `domain.ValidateNodeClaim` and `processes.UpsertClaim`
  refuse it on any other mode. The composer's Advanced sheet picks it from the
  routing set's staging nodes, and clone and copy carry it.
- **Dedicated spot:** every write transaction that touches claims ends with
  `processes.CheckKeepStagedSpots`, which refuses another claim naming a kept
  spot (any routing column within a style or across processes; staging or its
  own spot only across the styles of one process) and refuses moving or
  clearing a spot while open orders deliver to it. The claim's own inbound
  staging may be the spot. A lane is refused on it, as on every claim leg.
  Per Edge only. `plantspec.Validate` carries the twin for the seeder.

### `ClaimRole = "changeover"` — REMOVED (UI consistency refactor)
- **Status:** removed. Surviving evacuate-during-changeover mechanic is
  driven by `swap_mode` + `EvacuateOnChangeover` on the active claim.
- **DB verification:** 2026-05-24, plant ITPI returned 0 rows
- **Removal commit:** UI consistency refactor (squashed)
- **Notes:** if non-ITPI plants discover non-zero rows post-deploy, run
  a DELETE migration. The engine no longer has a branch for this role,
  so legacy rows would fail validation on the next claim load.
```

Add an entry every time something is "hidden" or "kept for compatibility."
Without this list, the next pass through the code can't tell what's
load-bearing vs. what's residue.

