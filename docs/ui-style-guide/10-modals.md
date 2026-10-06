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

### Sheets: one shell, one width, one Escape order

A page that opens several dialogs opens them through **one shell**. On the
Edge Processes page that is `openSheet` (`processes-desktop.js`): one
`.pd-modal` card on the `.modal-overlay` scrim, header, body, a footer with the
sheet's status line, Cancel and the action. Advanced, Generate variants and
every one-field sheet use the same card, so the desktop has one dialog and
not one per feature.

**One width, derived from the field the sheet holds.** A sheet field is a
230 px label column, a 14 px gap and a 420 px wide input; add 24 px of body
padding and a 1 px border each side and 18 px for the body's scrollbar when a
long sheet scrolls, and every sheet is **732 px**. A width set per sheet (there
were a 480 and a 600) sat under that sum and the input ran out of the card.
Below a 780 px window (the card plus the scrim's margin) the card is capped at
the window and each label stacks above its input instead of pushing into a
horizontal scroll. The one wider sheet is Generate variants (920 px), because
its body is a table with a column per claimed position, not a field.
`processes_desktop_sheet_width_test.go` reads the four rules from the
stylesheet and holds the width to their sum, so changing one without the
width goes red.

**One Escape order: innermost first.** One key handler (`onEscape`) walks the
layers the page stacks, innermost first, and acts on the first it finds: an
open popover closes — the page's (`#pd-pop`), Settings' group list
(`#pd-stpop`), or a picker a dialog opened over itself (`#pd-advpop`:
Advanced's, Generate variants', a sheet's group list); else, while the discard question is up, Escape does nothing;
else the open layer — Advanced, then Generate variants, then an `openSheet`
sheet — asks or closes (below); else, on the Flows tab, the selected position
is let go. An Escape that closed the sheet under an open menu left the menu
floating over nothing.

**A sheet with typed input asks before it discards.** An Escape on a sheet
nobody has typed into closes it. On a sheet with typed work it opens
*Discard the changes?* with **Keep editing**, which puts every field back
exactly as it was, and **Discard**, which closes the layer the way its own
Cancel does. Advanced, Generate variants and every `openSheet` sheet ask the
same question, and while it is up Escape does nothing: only its two buttons
answer it. "Typed" is read off the DOM, not kept in a ledger: a field whose
property (`value`, `checked`) differs from the attribute it was drawn with has
been edited. The question is for Escape — a key that elsewhere only closes
menus. Cancel is a deliberate click and discards without
asking; the backdrop does not close a sheet at all (the lifecycle default
above).

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

### Deep links: `?open=`

A thing with a pop-up is linkable by `?open=<key>` on its page:
`/orders?open=42`, `/robots?open=AMR-07`, `/nodes?open=ALN_003`,
`/bins?open=17`. Core reads it with one helper, `app.js` `openFromQuery`.

A pop-up that names an order, robot, node or bin links it this way — every
reference, not some of them. A link inside a clickable table row carries
`data-action="stopPropagation"` so it navigates instead of opening the row.
A link to a page behind login renders only for a viewer who is logged in;
for anyone else the name is plain text.

### What not to do

- ❌ `style="display:none"` toggled by JS — fragile, no transitions
- ❌ HTML5 `hidden` attribute — inconsistent browser styling
- ❌ Per-page modal markup — use the shared structure
- ❌ Inline `onclick="closeXModal()"` — use `data-action="close-modal"`

