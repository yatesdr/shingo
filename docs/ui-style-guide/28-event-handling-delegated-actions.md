## Event handling — delegated actions

**Decided: no inline event handlers in templates. Every
DOM event is mediated through `data-action[-event]` attributes and a
per-page `delegateActions` call.**

```html
<!-- GOOD: click handler -->
<button class="btn" data-action="deleteOrder:42">Delete</button>

<!-- GOOD: select with change handler -->
<select data-action-change="navigateToProcess">…</select>

<!-- GOOD: form submit handler -->
<form data-action-submit="submitPLCreate" method="POST" action="/payloads/create">…</form>

<!-- GOOD: data-* attributes for JSON or multi-field payloads -->
<button class="btn" data-action="editStyle" data-style="{{json .}}">Edit</button>

<!-- GOOD: backdrop close opt-in on the overlay element -->
<div class="modal-overlay" id="order-detail-modal" data-backdrop-close>...</div>

<!-- BAD -->
<button onclick="deleteOrder(42)">Delete</button>
<select onchange="navigateToProcess()">…</select>
```

### Attribute → event mapping

| Attribute | DOM event | Notes |
|---|---|---|
| `data-action` | `click` | Default; what you'll use 90% of the time |
| `data-action-change` | `change` | Selects, checkboxes, file inputs |
| `data-action-input` | `input` | Live-update on every keystroke |
| `data-action-blur` | `focusout` (bubbling form of blur) | Cell-commit on losing focus |
| `data-action-keydown` | `keydown` | Per-key handling (Enter/Escape commit/cancel) |
| `data-action-submit` | `submit` | Form-level — handler can call `evt.preventDefault()` |

Add a new event type by extending the `eventRe` in
`shingo-edge/www/inline_onclick_drift_test.go` and adding the
`data-action-<event>` mapping to `delegateActions` in
`shared/utils.js`.

### Convention

- `data-action="verb"` → handler called as `verb(el, evt)`
- `data-action="verb:arg"` → handler called as `verb("arg", el, evt)`
- `data-action="verb:a:b"` → handler called as `verb("a", "b", el, evt)`
- The dispatcher binds `this` to the matched element so the old
  `onclick="foo(this)"` semantics survive unchanged.
- JSON-shaped or multi-key payloads go in `data-*` attributes that
  the handler reads off `this.dataset`. The element is also the
  first positional argument.

### Built-in verbs and attribute conventions

- `stopPropagation` — calls `event.stopPropagation()` and returns.
  Lets a child cell with its own data-action exist inside a row
  handler without firing the row handler.
- `data-backdrop-close` on a `.modal-overlay` removes `.active`
  when the click target IS the overlay (not an inner element).
  Wired by `installBackdropClose()` from `shared/utils.js`,
  called once per surface at module load.
- `data-skip-on-checkbox="1"` on a row handler skips the dispatch
  when the click originated inside a checkbox cell — lets row-click
  and per-row checkbox actions coexist cleanly.
- `data-prevent-default="1"` calls `event.preventDefault()` before
  dispatch. Used for `<a href="#">` navigation that shouldn't
  navigate, and form submits handled via fetch().

### Drift test

`TestNoInlineEventHandlersInTemplates` in both `shingo-edge/www/`
and `shingo-core/www/` walks every embedded template file and fails
CI on any line containing `on<event>=` for click / change / input /
blur / keydown / submit / focus / keyup / mousedown / mouseup. The
allowlist is empty; future justified exceptions land there with a
comment.

### Per-page handler registration

Every page script ends with an explicit `delegateActions` call
listing the handler functions used by that page. The `events: [...]`
option binds the same map across multiple event types in one call.

```js
import { api, toast, delegateActions } from '/static/js/shingoedge.js';

async function deleteOrder(orderID) { … }
async function navigateToProcess(el) { window.location = '?process=' + el.value; }
function renderClaimForm() { … }

delegateActions(document.body, {
    deleteOrder,
    navigateToProcess,
    renderClaimForm,
    // …every handler the template's data-action[-event] attrs reference
}, { events: ['click', 'change', 'input', 'blur', 'keydown', 'submit'] });
```

Page scripts that need a different handler set for an HTMX-swapped
sub-container can call `delegateActions(swapTarget, {…})` with a
scoped root. The dataset sentinel prevents double-binding when the
swap target survives a re-fill.

