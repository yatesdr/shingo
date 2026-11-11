## JavaScript primitives

Verified against: 51489c78

### Use these helpers

Don't reimplement. The shared modules export these, one job per file, and a
page imports each helper from the file that defines it, at
`/static/shared/<file>`. No file re-exports another's helpers, so no page
reaches one module by two URLs (two URLs would be two instances: two clocks,
two buses).

#### `dom.js`: the HTML escape and the DOM helpers

<!-- generated: shared/dom.js -->

| Export | Kind | What it does |
|---|---|---|
| `escapeHtml` | function | Last-resort escape for legacy innerHTML concatenation. |
| `h` | function | Tagged-template HTML builder. |
| `el` | function | DOM element builder. |
| `delegateActions` | function | delegateActions wires event listeners on `root` that dispatch to entries in `handlerMap` based on the event target's nearest `[data-action]` (or `[data-action-<event>]`) ancestor. |
| `switchTab` | function | switchTab is the one tab handler (style guide §22). |
| `debounce` | function | Debounce — delays calling fn until ms after the last invocation. |

<!-- /generated -->

#### `clock.js`: the server's clock, the plant-local formatters, live durations

<!-- generated: shared/clock.js -->

| Export | Kind | What it does |
|---|---|---|
| `syncServerClock` | function | syncServerClock re-pins the origin. |
| `serverNow` | function | serverNow is the drop-in for Date.now() ANYWHERE the result is compared against a server-supplied timestamp. |
| `isSimClock` | function | isSimClock reports whether the server's now is simulated — for the marker that makes a sim page say so. |
| `simSpeed` | function | simSpeed is the multiplier, for the marker's readout. |
| `timeAgo` | function | — |
| `formatTime` | function | — |
| `formatClock` | function | formatClock renders time-of-day only, plant-local — the JS twin of planttime.Clock ("15:04"). |
| `formatDuration` | function | formatDuration is THE SAME LADDER as protocol.FormatDuration (Go), output byte for byte. |
| `installLiveDurations` | function | — |
| `renderLiveDurations` | function | Paint one frame; report whether anything remains to tick. |
| `installHtmxLiveDurations` | function | Re-render live durations after an htmx swap, as installHtmxTimestampConversion does for &lt;time data-utc&gt;. |
| `convertTimestamps` | function | Rewrite &lt;time data-utc="..."&gt; elements to plant-local. |
| `installHtmxTimestampConversion` | function | installHtmxTimestampConversion wires a document.body listener for `htmx:afterSwap` that re-runs convertTimestamps() against the swapped-in subtree (event.detail.target). |

<!-- /generated -->

#### `list.js`: table sort, keyed lists, the filter-bar store

<!-- generated: shared/list.js -->

| Export | Kind | What it does |
|---|---|---|
| `installTableSort` | function | installTableSort(root) wires click-to-sort onto any &lt;table data-sortable&gt; under `root` (default: document). |
| `reconcileList` | function | reconcileList — keyed create-or-update-or-remove for SSE-driven lists. |
| `createStore` | function | createStore — minimal observable backing the global filter bar. |

<!-- /generated -->

#### `utils.js`: HTTP, modals, confirm / prompt / toast, the SSE factory

<!-- generated: shared/utils.js -->

| Export | Kind | What it does |
|---|---|---|
| `api` | const | — |
| `apiResult` | const | apiResult is api's non-throwing twin: it always resolves to { ok, status, body }, so a 400 carrying errors{field: msg} reaches the page whole. |
| `createSSE` | function | Wraps EventSource with exponential backoff and build-id auto-reload. |
| `showRefreshBanner` | function | showRefreshBanner pins a non-modal banner at the top of the viewport telling the operator a new server build is available. |
| `showModal` | function | Open a modal. |
| `hideModal` | function | Hide a modal. |
| `confirm` | function | Promise-based async confirm. |
| `prompt` | function | Promise-based async prompt. |
| `toast` | function | Auto-dismissing toast notification. |
| `installBackdropClose` | function | installBackdropClose registers a single document-level click listener that closes any .modal-overlay element marked `data-backdrop-close` when the click target IS the overlay (not an inner .modal child). |
| `setSSEQuery` | function | setSSEQuery adds parameters to the stream's URL (the station board names its station, so the Edge marks its screen online on connect and heartbeat). |
| `setSSEWatchdog` | function | setSSEWatchdog arms the no-frame watchdog: ms with no frame of any kind and the stream is treated as dead. |
| `setSSEReloadOnBuild` | function | setSSEReloadOnBuild switches the bus's build-id-mismatch behavior from the dismissible refresh banner (admin pages) to an immediate location.reload() (kiosk pages — no operator to dismiss; the right move is to adopt the new Core build). |
| `onSSE` | function | — |
| `closeSSEBus` | function | closeSSEBus — tear down the shared EventSource (page unload / tests). |
| `durationFromText` | function | durationFromText is what the page sends for a duration box: the typed text ("45 min", or a Go string typed in) as a Go duration string. |
| `durationToText` | function | durationToText is what the box shows for a stored Go duration: one box with its unit in it ("45 min", "10 s", "1 h", "1 h 30 min"), never "45m0s". |
| `collectList` | function | collectList reads a list section from the rows that are on the page, in order, NOT by index: removing row 0 must never lose the rows after it (an index walk stops at the first missing index). |
| `settingsPage` | function | settingsPage binds a settings page's draft, save bar and answers. |

<!-- /generated -->

`shared/esc.js` exports `esc`, the same escape with no imports and no DOM, for
code that must not pull in all of `utils.js` (the station's renderers, the node
tests).

### Module shape

**ES modules.** Shared utilities and page JS use `import`/`export`, and their
script tags carry `type="module"`: an explicit import is a dependency a reader
and a linter can see, where a `window.X` global is not. Edge admin page scripts
import from `/static/js/shingoedge.js`, which re-exports `escapeHtml` and
`delegateActions` from `dom.js` but carries its own `api`, `createSSE`,
`showModal` / `hideModal`, `confirm` / `prompt` and `toast`. The classic scripts left
are the ones that must run before any module: the theme pre-paint in each
layout's head, the clock globals partial, vendored bundles, and the glyph file
that ships verbatim (`composer-glyphs.js`).

### HTML construction

Always prefer `h\`\`` over string concatenation:

```js
// GOOD
container.innerHTML = h`<div class="row">${name}</div>`;

// BAD — manual escaping, easy to miss one
container.innerHTML = '<div class="row">' + escapeHtml(name) + '</div>';
```

`h\`\`` auto-escapes interpolations, joins arrays without escape, supports an
opt-out for pre-built safe HTML (`{ __html: true, value: safe }`).

### Avoid

- Raw `innerHTML += '...'` with concatenated user data
- Bare `fetch()` — use `api.*` for consistent error handling
- Bare `EventSource` — use `createSSE` for reconnect + build-id detection

