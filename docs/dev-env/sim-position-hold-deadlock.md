# The permanent position hold — an unclaimed staged bin wedges a robot for good

Found 2026-09-06 while measuring the sim speed ceiling. **Pre-existing**: the
position gate and `holdForPosition` are untouched by the clock work that found
it. Speed does not cause it; speed only makes it arrive sooner and more often.

## What it looks like

Orders sit `in_transit` forever with a robot assigned, while most of the fleet
is idle. On the observed run, at sim 10:02:

```
35  move BRKT     in_transit  AMR-18   last event sim 07:56  (2h 06m, no change)
42  move PANEL-B  in_transit  AMR-05   last event sim 07:58  (2h 04m, no change)
40  move PANEL-B  reshuffling          "dig 42 is working this lane · 2h 04m"
```

Transit is 15–20 simulated seconds. These had been in transit for two simulated
hours. Meanwhile **14 of 20 robots were free** — so this is not fleet exhaustion,
and the queue of orders behind it ("waiting for an empty bin", 1h+) is not
waiting for a robot.

## The mechanism

The discriminator is in the driver's own log line. Seven position holds occurred
in the run; five cleared, two never did:

```
CLEARED:  sg-31  HOLDING at PLN_003 — PLN_003 holds bin 18 (claimed by order 29)
CLEARED:  sg-162 HOLDING at PLN_004 — PLN_004 holds bin 19 (claimed by order 158)
CLEARED:  sg-258 HOLDING at PLN_004 — PLN_004 holds bin 49 (claimed by order 257)
CLEARED:  sg-281 HOLDING at PLN_003 — PLN_003 holds bin 50 (claimed by order 280)
NEVER:    sg-35  HOLDING at ALN_004 — ALN_004 holds bin 7 (claimed by nobody)
NEVER:    sg-42  HOLDING at ALN_003 — ALN_003 holds bin 6 (claimed by nobody)
```

**"claimed by nobody" is the whole finding.** A hold behind a bin some other
order owns is a queue: that order finishes, moves its bin, the position clears,
and `holdForPosition` logs "resumed". A hold behind a bin nobody owns is a
deadlock, because there is no one to do the moving.

`holdForPosition` (`shingo-core/fleet/simulator/driver.go:577`) re-arms
`p.deadline = now.Add(time.Second)` and returns true. It has no give-up, no
escalation and no bound — by design, since every hold it was written for was the
first kind. The robot stays acquired for the life of the process.

And the bins really are ownerless and really are permanent:

```
 id |   label     | status |  node   | payload |  updated_at (sim)
  6 | BIN-PB-03   | staged | ALN_003 | PANEL-B | 07:58:07
  7 | BIN-BRKT-01 | staged | ALN_004 | BRKT    | 07:57:00
        reservations for bins 6, 7: (0 rows)
```

`staging.ttl: 0s` means PERMANENT, and `ReleaseExpiredStaged`
(`store/bins/bins.go:1202`) skips NULL-expiry rows by construction. A bin left
`staged` with no claim and no expiry is never released by anything.

## It ratchets, which is why it dominates a long run

Every occurrence is permanent, so the count only goes up: each one costs one
robot and blocks one lineside position for the life of the process. Orders
needing that lane queue behind it forever, which is the "waiting for an empty
bin" sediment (261, 284, 291, 303) and the "waiting for partner robot" pair
(300, 441) on the same board. A rig left running degrades monotonically —
which is exactly what makes it look like a speed problem when it is not.

## It contradicts the ruling recorded in shingocore.dev.yaml

The `staging.ttl: 0s` block states that two diagnostic passes on 2026-08-30/31
agreed that at ttl 0s "the freeze blocks ZERO orders", that every starved order
is refused by `EmptyCarrierWhere`'s `COALESCE(b.payload_code,'') = ''` clause,
and therefore that "frozen carriers are an amplifier, not a wedge."

**On this run a frozen carrier is the wedge.** Not through the empty-carrier
query — bins 6 and 7 carry PANEL-B and BRKT, so that clause would never select
them and the passes that looked there could not have seen this. They wedge
through the **position gate**, by occupying a lineside node another order has to
place onto. That is a different code path from the one the ruling was argued on,
so the ruling is not wrong about what it examined; it is incomplete about what
`ttl: 0s` costs.

Recorded, not reconciled: the 30 s TTL is not being restored here. The yaml's
argument against it — that it flips a lineside carrier back to `available` while
it is still at the cell, where `AccessibleEmptyOrder` ranks it ahead of every
lane mouth and manufactures keeper theft — is untouched by this finding and
still stands. **Both are true**, which means the answer is neither value but a
release path that is aware of who owns the bin.

## What would fix it, in preference order

1. **Give the hold a bound.** `holdForPosition` should escalate after N ticks
   rather than re-arming forever — release the robot, fail the leg, and log the
   blocking bin. Cheapest, entirely inside the sim driver, and turns a silent
   permanent wedge into a visible failure. It does not fix the cause.
2. **Find who orphans the bin.** A bin reaching `staged` with no reservation is
   the actual defect; bins 6 and 7 got there somehow. That is the real fix and
   it needs its own investigation — see `shingo_sim_consume_node_placement_deadlock`,
   whose "orphaned bin claim self-deadlocks its owner" is the sibling shape
   (there the claim exists and is wrong; here it is absent).
3. **An owner-aware release** for staged bins with no claim, which is the middle
   ground the yaml's two rejected values are arguing across.

## How to detect it in one query

```sql
SELECT b.id, b.label, b.status, n.name AS node
FROM bins b JOIN nodes n ON n.id = b.node_id
LEFT JOIN reservations r ON r.bin_id = b.id
WHERE b.status = 'staged' AND r.bin_id IS NULL;
```

Any row is a position that will never clear. Cross-check against
`grep 'HOLDING at' core.log | grep 'claimed by nobody'`.
