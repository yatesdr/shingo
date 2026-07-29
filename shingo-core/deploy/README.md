# Deploy artifacts

Files installed onto a plant core box.

| File | Installed to | By |
|---|---|---|
| `shingo-core.service` | `/etc/systemd/system/shingo-core.service` | `install-core.sh` |
| `journald-shingo.conf` | `/etc/systemd/journald.conf.d/10-shingo.conf` | **by hand** — see below |

## Journal retention (`journald-shingo.conf`)

`install-core.sh` does not install this one. journald has no per-unit
retention, so the file changes how the whole host keeps logs — including
whatever else runs on the box. That is an operator's call, not an
installer's, so the installer only points at it.

To apply:

```bash
sudo cp shingo-core/deploy/journald-shingo.conf /etc/systemd/journald.conf.d/10-shingo.conf
sudo systemctl restart systemd-journald
journalctl --disk-usage
```

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
   therefore journald. It defaults to everything except `countgroup` and
   `rds`, which were 334,361 and 125,817 lines/day — 75% of the journal
   between them. The count-group poll additionally logs on occupancy
   *change* rather than per 500 ms tick. Neither poller was disabled;
   only their tracing was.

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
