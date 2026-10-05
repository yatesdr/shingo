## JavaScript primitives

### Use these helpers

Don't reimplement. The shared module at `shared/utils.js` exports:

```js
import {
  // HTML construction
  escapeHtml,    // last-resort string escape
  h,             // tagged template — auto-escapes interpolations
  el,            // DOM builder — el(tag, props, children)

  // HTTP
  api,           // api.get(url), api.post(url, body), .put, .delete

  // Time
  timeAgo,       // relative ("3m ago")
  formatTime,    // full datetime, plant-local labeled (see Timestamps below)
  formatClock,   // time-of-day only ("15:04"), plant-local
  formatDuration,
  convertTimestamps, // rollover shim — old UTC paints only (see Timestamps)

  // SSE
  createSSE,     // EventSource with backoff + build-id reload

  // Modals & dialogs
  showModal, hideModal, confirm, toast, prompt,

  // Misc
  debounce,
} from '/static/shared/utils.js';
```

### Module shape

**Decided: ES modules.** All shared utilities and consuming JS use
`import`/`export`. Script tags get `type="module"`. This matches the
operator station's existing pattern. Core's bare globals and Edge's IIFE
wrap get migrated to modules as part of the refactor.

Rationale: operator station already uses modules successfully; the
three-pattern divergence collapses to one; modern tooling (linters,
formatters, possible future TypeScript, bundlers) assumes modules; AI
agents parse explicit `import` statements more reliably than implicit
`window.X` globals. The cost is a one-time pain (script tag changes,
loading semantics shift to deferred-by-default) instead of perpetual
maintenance of the divergence.

Browser support: ES modules require Chromium 60+ / Firefox 60+ / Safari
11+ (2017-2018 vintage). Modern plant kiosks should be fine; verify the
oldest device in the field before shipping.

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

- ❌ Raw `innerHTML += '...'` with concatenated user data
- ❌ Bare `fetch()` — use `api.*` for consistent error handling
- ❌ Bare `EventSource` — use `createSSE` for reconnect + build-id detection

