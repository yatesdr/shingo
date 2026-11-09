# Deploy artifacts

Verified against: 2cbdbd5b

Files installed onto a plant core box.

| File | Installed to | By |
|---|---|---|
| `shingo-core.service` | `/etc/systemd/system/shingo-core.service` | `install-core.sh` |
| [`deploy/journald-shingo.conf`](../../deploy/journald-shingo.conf) | `/etc/systemd/journald.conf.d/10-shingo.conf` | `install-core.sh` and `install-edge.sh` |
| [`deploy/shingo-debug.logrotate`](../../deploy/shingo-debug.logrotate) | `/etc/logrotate.d/shingo-debug` | `install-core.sh` (and `install-edge.sh` on edge boxes) |

Both installers copy the two host files with the same idempotent cmp-skip
pattern: a file is copied only when the installed copy differs from the repo
copy (and `systemd-journald` restarted only then), so a fresh install and an
update land the same state with no manual step.

## Debug-file rotation (`shingo-debug.logrotate`)

Both units run `--log-debug`, so both processes mirror every subsystem to
`/opt/shingo/shingo-debug.log`. The file is truncated on open
(`protocol/debuglog`), so each run starts empty — but between deploys it
grows unbounded, and months-long runs are the goal. logrotate handles it:
daily at `maxsize 50M`, 14 rotations compressed (`copytruncate`, because the
process never reopens the fd).

## Journal retention (`journald-shingo.conf`)

Journal retention is a decision the installer makes, not an accident of
filesystem size. journald's built-in default caps the journal at 10% of the
filesystem or 4G, whichever is smaller, with no time bound — so how far back an
incident can be read would depend on how busy the box is. The drop-in sets all
three knobs explicitly: 4G in total, 90 days, 128M per journal file.

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

### Volume first, then retention

Retention only buys days if the volume is bounded, so the journal-volume knob
is the application's, not the host's: `logging.stderr_subsystems` in
`shingocore.yaml` gates which debuglog subsystems reach stderr, and therefore
journald. It defaults to everything except `rds`; the rds poller still runs,
only its tracing is out of the journal. Muted subsystems stay fully visible on
the Logs page, which reads the in-memory ring buffer and is not gated at all.
The key's values, including the `[all]` escape hatch for an incident, are in
[Configuration](../docs/configuration.md).
