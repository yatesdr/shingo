## JavaScript primitives

Verified against: 74c3eb77

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

#### `settings.js`: the settings page

<!-- generated: shared/settings.js -->

| Export | Kind | What it does |
|---|---|---|
| `durationFromText` | function | durationFromText is what the page sends for a duration box: the typed text ("45 min", or a Go string typed in) as a Go duration string. |
| `durationToText` | function | durationToText is what the box shows for a stored Go duration: one box with its unit in it ("45 min", "10 s", "1 h", "1 h 30 min"), never "45m0s". |
| `collectList` | function | collectList reads a list section from the rows that are on the page, in order, NOT by index: removing row 0 must never lose the rows after it (an index walk stops at the first missing index). |
| `settingsPage` | function | settingsPage binds a settings page's draft, save bar and answers. |

<!-- /generated -->

#### `bus.js`: the live bus

<!-- generated: shared/bus.js -->

| Export | Kind | What it does |
|---|---|---|
| `setSSEQuery` | function | setSSEQuery adds parameters to the stream's URL (the station board names its station, so the Edge marks its screen online on connect and heartbeat). |
| `setSSEWatchdog` | function | setSSEWatchdog arms the no-frame watchdog: ms with no frame of any kind and the stream is treated as dead. |
| `sseStatus` | function | sseStatus is the link as the bus last saw it: {live, lastFrameAt}, live while the stream is up, lastFrameAt the browser time (ms) of its last frame. |
| `setSSEReloadOnBuild` | function | setSSEReloadOnBuild switches the bus's build-id-mismatch behavior from the 'build-changed' dispatch (admin pages show the refresh banner on it) to an immediate location.reload() (kiosk pages — no operator to dismiss; the right move is to adopt the new Core build). |
| `onSSE` | function | — |
| `closeSSEBus` | function | closeSSEBus — tear down the shared EventSource (page unload / tests). |

<!-- /generated -->

#### `esc.js`: the escape the station's renderers use

`esc` is an escape with no imports and no DOM, for the station's renderers and
the node tests. It leaves `'` alone where `escapeHtml` escapes it, so the two
are not one helper; `dom.js`'s header says why they stay in two files.

<!-- generated: shared/esc.js -->

| Export | Kind | What it does |
|---|---|---|
| `esc` | function | esc escapes a value for either a text node or a double-quoted attribute. |

<!-- /generated -->

#### `layer.js`: the one overlay stack

Modals, sheets, confirm, prompt and popovers ([Modals](10-modals.md)).

<!-- generated: shared/layer.js -->

| Export | Kind | What it does |
|---|---|---|
| `openLayer` | function | openLayer registers node as the innermost entry and shows it. |
| `closeLayer` | function | closeLayer closes an entry, by its handle or its element, as a deliberate close does: no question asked. |
| `layerDepth` | function | layerDepth is how many entries are open. |
| `showModal` | function | showModal opens a markup modal (.modal-overlay, shown by .active) as an entry. |
| `hideModal` | function | hideModal is the deliberate close: it never asks. |
| `confirm` | function | confirm resolves true on its OK and false on anything else that closes it: Cancel, Escape, the backdrop where opts.backdrop opts in, a page closing it. |
| `prompt` | function | prompt resolves the typed text on OK or Enter, and null on anything else. |
| `confirmWithInput` | function | confirmWithInput is a confirm with an optional free-text field (Core's uiConfirmWithInput): {ok, value}, ok false on anything but its OK. |
| `openSheet` | function | openSheet is the Processes page's sheet shell, moved: a card with a title, a body, a status line and Cancel and OK. |

<!-- /generated -->

#### `notice.js`: the toast and the banner

([Dialog UX](11-dialog-ux-confirmation-prompt-toast.md)).

<!-- generated: shared/notice.js -->

| Export | Kind | What it does |
|---|---|---|
| `toast` | function | toast shows one message and returns its element. |
| `banner` | function | banner shows, or redraws in place, the banner under key, and returns its element. |
| `clearBanner` | function | clearBanner takes the banner under key down; no banner is no error. |
| `showNewBuildBanner` | function | showNewBuildBanner says the server runs a new build, with Refresh. |
| `showLostConnectionBanner` | function | showLostConnectionBanner says the page's stream to server ('core' or 'edge') dropped, and when it last answered: lastFrameAt is the browser time of the stream's last frame (bus.js sseStatus), null when none came. |
| `clearLostConnectionBanner` | function | clearLostConnectionBanner takes it down: the stream is back. |

<!-- /generated -->

#### `http.js`: the HTTP door

(Below.)

<!-- generated: shared/http.js -->

| Export | Kind | What it does |
|---|---|---|
| `REQUEST_TIMEOUT_MS` | const | — |
| `send` | function | send is the door. |
| `http` | const | http is the door by verb. |
| `hookHtmxErrors` | function | hookHtmxErrors puts htmx's failed requests on the same notice the door uses: a 4xx/5xx partial (htmx:responseError), no answer (htmx:sendError) and htmx's own timeout (htmx:timeout). |

<!-- /generated -->

#### `picker.js`: the picker's shell

([Picker](34-picker.md)).

<!-- generated: shared/picker.js -->

| Export | Kind | What it does |
|---|---|---|
| `createPicker` | function | createPicker draws a picker into host (its content replaced) and returns it. |

<!-- /generated -->

#### What each hub keeps

The hubs re-export none of these; a page imports each from its file.

- **Core, `/static/app.js`** (layout.html, every admin page): the door verbs
  Core's pages were written to, `apiGet` / `apiPost` / `apiPut` / `apiDelete`,
  over `http.js` with the notice off; a refusal rejects an Error named
  `ApiError` whose string is the door's sentence. Page verbs: `timeAgoHTML`,
  `setText`, `openFromQuery`, `enterSubmits`, `removeParentElement`,
  `removeClosestRow`, `toggleVisibility`, and the submit gates
  `confirmDeleteForm` / `containmentForm`. At load: table sort, the bus
  subscription every page holds, the new-build banner on `build-changed`.
- **Edge admin, `/static/js/shingoedge.js`**: its `api` (`get`, `post`, `put`,
  `patch`, `del`) over `http.js` with the notice off, a refusal rejected as an
  Error carrying the door's sentence; `refreshOnEvents` (an htmx
  partial refreshed from the bus), `navigateToProcess` /
  `navigateToProcessOrOrders`. At load: the new-build banner on
  `build-changed` and `hookHtmxErrors`, so a failed htmx request is on the
  notice.

### The HTTP door

`shared/http.js` is the one HTTP door, and it never throws. `send(method, url,
body, opts)`, and `http.get`, `post`, `put`, `patch`, `del`, resolve to
`{ ok, status, body, error, fieldErrors }`:

- `status` 0 is no answer: no connection, or the request's deadline passed.
- A 204 or an empty 2xx is `ok` with `body` null.
- `error` is one sentence on every failure: the server's `error` (the Edge's
  `{"error"}`, Core's configRefusal and kanbanHeld), else its `message`, else an
  `http.Error` text line, else one made from the status (`Refused (400)`,
  `Server error (500)`). A `null` or non-JSON body still has one.
- `fieldErrors` is `{field: message}`: configRefusal's `errors`, or a
  `field_errors` list.
- A lapsed session is `{ ok: false, status: 401, error: 'Signed out' }`: a
  redirect to `/login`, HTML where JSON was asked, or a 401.

**A failure shows once, from the door.** It puts the error on the notice
(`shared/notice.js`), and a lapsed session's notice carries the **Sign in**
link back to the page. A caller that draws the error inline passes
`{ notice: false }` for that call; a 401 goes on the notice even then (only the
notice has Sign in), and the answer carries `shown: true`, so the caller's own
line adds nothing to its "failed" or "not saved". A write given no body sends
`{}`; `null` sends none.

**Every request has a deadline**, `REQUEST_TIMEOUT_MS` (30 s, the operator
station's own): its own AbortController and timer, cleared when the answer is
read. `opts.timeout` changes it for one call. A caller's `opts.signal` that
aborts resolves `{ aborted: true }` and shows nothing.

`hookHtmxErrors(root)` puts htmx's failed requests (`htmx:responseError`,
`htmx:sendError`, `htmx:timeout`) on the same notice and returns the function
that takes it off again. The Edge hub installs it.

`TestSharedComponentSpecs` holds each rule; `TestSharedComponentPins` runs
the helper pins' api cases on the door.

### Module shape

**ES modules.** Shared utilities and page JS use `import`/`export`, and their
script tags carry `type="module"`: an explicit import is a dependency a reader
and a linter can see, where a `window.X` global is not. A page takes each
shared helper from its file and only its hub's own verbs from the hub (above).
The classic scripts left
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
- Bare `fetch()` — use the door (`http.js`), or the hub's api verbs, for one
  error sentence and the lapsed session
- Bare `EventSource` — use the bus (`onSSE`, `bus.js`) for one stream per tab,
  reconnect and build-change detection

