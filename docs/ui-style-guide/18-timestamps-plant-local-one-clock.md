## Timestamps — plant-local, one clock

Verified against: 74c3eb77

**Storage is UTC, the wire is RFC3339-UTC, and every rendered timestamp is
plant-local and labeled. The server's first paint is correct and final — no
post-paint rewrite.**

The convention, one rule each layer:

- **Server (Go)** — templates call `formatTime` / `formatTimePtr` /
  `formatClock` / `formatClockSeconds` (and Core's `formatDayClock`), all
  backed by `shared/planttime` with the binary's resolved `plantLocation`.
  Core resolves it from the `PLANT_TIMEZONE` environment variable, then the
  config's `timezone:`, else UTC (`planttime.DefaultZone`). Edge reads its
  config's `timezone:`; a blank one is filled from the zone Core offers on a
  heartbeat ack (`adoptPlantTimezone`, applied at restart), else UTC. An unset
  zone is UTC on purpose: visibly foreign, rather than plausibly local and
  wrong. Full datetimes render `Sep 5, 2026 09:23 CDT` inside
  `<time data-utc="2026-09-05T14:23:01Z">` — plant-local text, machine instant
  in the attribute.
- **Client (JS)** — `shared/clock.js` `formatTime` / `formatClock` read
  `window.PLANT_TZ`, inlined synchronously by each module's
  `templates/partials/clock-globals.html` (with `window.SHINGO_CLOCK`, the
  server's clock) as `window.PLANT_TZ = "{{ plantTZ }}"`. JS-rendered rows
  therefore land on the SAME clock the server painted — one screen, one clock.
  `TestEveryClockPageCarriesGlobals` (Core and Edge) fails a page whose scripts
  reach a clock function without the partial. The JS `formatTime` is
  byte-for-byte the Go layout's twin; change them in both or in neither (pinned
  by `shared/testdata/js/utils.planttime.test.js`).
- **Wire and storage stay UTC.** `data-utc` attributes, `data-since` /
  `data-until` machine readers (robots, dashboards, orders), API JSON —
  all RFC3339-UTC. Only display converts.
- **`convertTimestamps` is a rollover shim, not a converter.** It rewrites
  only old UTC-painted text (trailing ` UTC`), marks the node
  `data-converted`, and never touches a plant-local server paint — that
  would double-convert it.
- **Date filters resolve plant-local at the server**: a bare
  `YYYY-MM-DD` `since`/`until` means the plant's calendar day.
- **DST** — both `MST` spellings in the layout and `Intl`'s zone database
  handle spring-forward/fall-back; fall-back's ambiguous hour is
  disambiguated by the zone abbreviation in the label, which is why the
  label is not optional decoration.

**Core's plant-clock module.** What `shared/clock.js` does not carry, and Core
alone computes, lives in one Core module,
`shingo-core/www/static/components/plantclock.js`: the plant date
(`plantDate`), the clock with seconds (`formatClockSeconds`), the range window
(`windowFor`, from `serverNow()` on the plant calendar), the chart bucket label
(`bucketLabel`) and the in-progress test (`inProgress`). It reads `PLANT_TZ`
with the same fallback as `clock.js` and has no "now" of its own. It is Core's
because only Core computes any of it (clause 2 of
[shared-layer-promotion.md](../shared-layer-promotion.md)). It is pinned against
`formatClock` and `formatTime` by `plantclock.test.js` (`TestPlantClockJS`), and
Core's `clock_globals_drift_test.go` holds three rules: no page module reads the
browser's calendar (`TestNoPageModuleReadsTheBrowserCalendar`), none defines a
duration formatter (`TestNoPageModuleDefinesADurationFormatter`), and
`PLANT_TZ` is read in exactly `shared/clock.js` and `plantclock.js`
(`TestPlantTZReadInTwoFiles`).

**Rows that share a minute show seconds.** In a list where several rows can
carry the same minute (an order's history, a mission's stages), a time to the
minute makes order and gaps unreadable. Use the seconds clock there.

**A relative stamp carries the absolute time.** `"1h ago"` gets the full
plant-local time in its `title` (`app.js timeAgoHTML`), so ten rows of
"1h ago" can still be told apart.

### Timestamp anti-patterns

- A new surface painting UTC and "fixing it later" — a UTC first paint
  flickers when the client rewrites it
- `new Date(x).toLocaleString()` in page JS — bypasses `PLANT_TZ`,
  lands on the viewer's zone, disagrees with the server paint on the same
  screen
- Formatting datetimes in handlers (Go string-building) — use the
  template funcs or `planttime` directly so every surface shares one
  implementation

