## Modals

### One mechanism

Pick **Core's `.modal-overlay` + `.active` class** pattern. CSS-driven,
theme-aware, no inline `style.display` toggling, no DOM race conditions.

```html
<div class="modal-overlay" id="my-modal">
  <div class="modal">
    <div class="modal-header">
      <h2>Modal Title</h2>
      <button class="modal-close" data-action="close-modal">&times;</button>
    </div>
    <div class="modal-body">
      <!-- content -->
    </div>
    <div class="modal-footer">
      <button class="btn" data-action="close-modal">Cancel</button>
      <button class="btn btn-primary" data-action="save">Save</button>
    </div>
  </div>
</div>
```

```js
import { showModal, hideModal } from '/static/shared/modal.js';
showModal('my-modal');
hideModal('my-modal');
```

### Lifecycle contract — decided defaults

| Behavior | Default | Opt-in override |
|---|---|---|
| Open | `showModal(id)` adds `.active` class | — |
| Close | `hideModal(id)` removes `.active` class | — |
| **Backdrop click** | **Does NOT close** (button-only dismissal — safer for data-input modals) | `showModal(id, { closeOnBackdrop: true })` for info/confirm modals |
| **Escape key** | Closes (same effect as clicking the X) | — |
| **Form state on close** | **Cleared** (no stale data on reopen) | `hideModal(id, { preserveState: true })` for wizards / edit-flows |

The pair of defaults (button-only-close + clear-on-close) work together:
closing a modal — by any deliberate means — discards state, and closing
requires a deliberate action. Accidental clicks on the backdrop don't
silently nuke the user's work.

**When to opt into `closeOnBackdrop: true`:** info modals, simple
confirmations, anything where state preservation isn't a concern and
quick dismissal is a UX win.

**When to opt into `preserveState: true`:** multi-step wizards, long
edit forms where accidental close-then-reopen shouldn't lose work.
Concrete examples: Core's bins cycle-count wizard, test-orders command
form.

Most modals — the claim editor, station editor, anything with serious
input — get the safe defaults automatically. The combo of "button-only
close + clear-on-close" means an accidental backdrop click does nothing,
and a deliberate close starts the next session fresh.

### Touch variant

Operator HMI uses the same mechanism with a `.modal--touch` modifier for
sizing:

```css
.modal--touch .modal {
  min-width: 480px;
  font-size: 16px;
}
.modal--touch button { min-height: var(--os-touch-min); }
```

### What not to do

- ❌ `style="display:none"` toggled by JS — fragile, no transitions
- ❌ HTML5 `hidden` attribute — inconsistent browser styling
- ❌ Per-page modal markup — use the shared structure
- ❌ Inline `onclick="closeXModal()"` — use `data-action="close-modal"`

