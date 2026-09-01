# Deploy artifacts

Files installed onto a plant core box.

| File | Installed to | By |
|---|---|---|
| `shingo-core.service` | `/etc/systemd/system/shingo-core.service` | `install-core.sh` |
| `../deploy/journald-shingo.conf` | `/etc/systemd/journald.conf.d/10-shingo.conf` | `install-core.sh` and `install-edge.sh` (both tiers since 2026-09-01; was by-hand before that) |
| `../deploy/shingo-debug.logrotate` | `/etc/logrotate.d/shingo-debug` | `install-core.sh` (and `install-edge.sh` on edge boxes) |

## Debug-file rotation (`shingo-debug.logrotate`)

Both units run `--log-debug`, so both processes mirror every subsystem to
`/opt/shingo/shingo-debug.log`. The file is truncated on open
(`protocol/debuglog`), so each run starts empty — but between deploys it
grows unbounded, and months-long runs are the goal. logrotate handles it:
daily at `maxsize 50M`, 14 rotations compressed (`copytruncate`, because the
process never reopens the fd). The installers keep it current; no manual
step.

## Journal retention (`journald-shingo.conf`)

Installed by **both** installers (core and edge) since 2026-09-01, same
idempotent cmp-skip pattern as the logrotate config: copied to
`/etc/systemd/journald.conf.d/10-shingo.conf` and `systemd-journald`
restarted only when the installed copy differs from the repo copy. New
plants get retention automatically on first install; existing plants get
it on the next update.

It was by-hand until 2026-09-01 — the original reasoning was that a
host-wide log policy is an operator's call, not an installer's. That lost
to the other failure mode: every existing plant kept drifting (HK core sat
at 3.9 GB with no time bound four months in), and a deploy-time reminder
nobody reads is not automation. The 4G/90day/128M values match what
systemd's implicit default was already doing on disk size; the drop-in
makes them explicit and adds the time bound.

To check what a box is doing today:

```bash
# Configured (all-commented output means the built-in default is in force:
# cap at 10% of the filesystem or 4G, whichever is smaller; no time bound)
grep -E 'SystemMaxUse|MaxRetentionSec|SystemMaxFileSize' \
    /etc/systemd/journald.conf /etc/systemd/journald.conf.d/* 2>/dev/null

# Actual
journalctl --disk-usage
journalctl -u shingo-core -o short-iso | head -1   # oldest line retained
```

### Why

Springfield, 2026-07-25: 3.8 GB of journal holding **~15 days**, oldest
retained line 2026-07-10 — while the incident under investigation was
2026-07-21 and the window being queried was 30 days. The evidence had
already rotated out. All three retention knobs were commented out, so the
number was an accident of filesystem size rather than a decision.

Two changes fixed it, and the order matters:

1. **Stop producing the volume.** `logging.stderr_subsystems` in
   `shingocore.yaml` gates which debuglog subsystems reach stderr, and
   therefore journald. It defaults to everything except `rds`, which was
   125,817 lines/day. The other half of that volume was a 500 ms
   occupancy poll writing 334,361 lines/day; that feature has since been
   retired outright, so the traffic is gone rather than muted. The rds
   poller was not disabled; only its tracing was.

   Escape hatch, no rebuild required — restore the full firehose for one
   incident with:

   ```yaml
   logging:
     stderr_subsystems: [all]
   ```

   and set it back afterwards. Muted subsystems are still fully visible in
   the browser log UI, which reads the in-memory ring buffer and is not
   gated at all.

2. **Then decide the retention** with this drop-in. Doing it in the other
   order just buys a bigger pile of poll traces.
