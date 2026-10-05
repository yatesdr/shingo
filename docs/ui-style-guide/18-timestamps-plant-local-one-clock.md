## Timestamps — plant-local, one clock

**Decided: storage is UTC, the wire is RFC3339-UTC, and every rendered
timestamp is plant-local and labeled. The server's first paint is correct
and final — no post-paint rewrite.**

The convention, one rule each layer:

- **Server (Go)** — templates call `formatTime` / `formatTimePtr` /
  `formatClock` / `formatClockSeconds` / `formatDayClock`, all backed by
  `shared/planttime` with the binary's resolved `plantLocation`
  (core: config `timezone:` with `America/Chicago` default; edge: config
  `timezone:`, empty falls back to UTC so a misconfigured box is visibly
  wrong rather than silently on the OS zone). Full datetimes render
  `Sep 5, 2026 09:23 CDT` inside `<time data-utc="2026-09-05T14:23:01Z">`
  — plant-local text, machine instant in the attribute.
- **Client (JS)** — `shared/utils.js` `formatTime` / `formatClock` read
  `window.PLANT_TZ`, inlined synchronously by `layout.html` (core) and
  `header.html` (edge) as `window.PLANT_TZ = "{{ plantTZ }}"`. JS-rendered
  rows therefore land on the SAME clock the server painted — one screen,
  one clock. The JS `formatTime` is byte-for-byte the Go layout's twin;
  change them in both or in neither (pinned by
  `shared/utils.planttime.test.js`).
- **Wire and storage stay UTC.** `data-utc` attributes, `data-since` /
  `data-until` machine readers (robots, dashboards, orders), API JSON —
  all RFC3339-UTC. Only display converts.
- **`convertTimestamps` is a rollover shim, not a converter.** It rewrites
  only old UTC-painted text (trailing ` UTC`), marks the node
  `data-converted`, and never touches a plant-local server paint — that
  would double-convert mid-deploy. Once no pre-plant-local markup remains
  in any served page it can be deleted.
- **Date filters resolve plant-local at the server** (Q-004): a bare
  `YYYY-MM-DD` `since`/`until` means the plant's calendar day.
- **DST** — both `MST` spellings in the layout and `Intl`'s zone database
  handle spring-forward/fall-back; fall-back's ambiguous hour is
  disambiguated by the zone abbreviation in the label, which is why the
  label is not optional decoration.

### Timestamp anti-patterns

- ❌ A new surface painting UTC and "fixing it later" — that is the
  flicker defect this convention closed
- ❌ `new Date(x).toLocaleString()` in page JS — bypasses `PLANT_TZ`,
  lands on the viewer's zone, disagrees with the server paint on the same
  screen
- ❌ Formatting datetimes in handlers (Go string-building) — use the
  template funcs or `planttime` directly so every surface shares one
  implementation

