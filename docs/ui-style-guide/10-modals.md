## Modals

Verified against: 74c3eb77

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
import { showModal, hideModal } from '/static/shared/layer.js';
showModal('my-modal');
hideModal('my-modal');
```

### Lifecycle contract — decided defaults

| Behavior | Default | Opt-in override |
|---|---|---|
| Open | `showModal(id)` adds `.active` class | — |
| Close | `hideModal(id)` removes `.active` class | — |
| **Backdrop click** | **Does NOT close** (button-only dismissal — safer for data-input modals) | `data-backdrop-close` on the overlay (read by `showModal` when it opens it; no install call), or `showModal(id, { closeOnBackdrop: true })`, for info/confirm modals |
| **Escape key** | Closes the innermost open entry, asking first when it holds typed work (the layer, below); a page does not close its modals from its own Escape handler | — |
| **Form state on close** | **Cleared** by `hideModal` (no stale data on reopen) | `hideModal(id, { preserveState: true })` for wizards / edit-flows |

The pair of defaults (button-only-close + clear-on-close) work together:
closing a modal — by any deliberate means — discards state, and closing
requires a deliberate action. Accidental clicks on the backdrop don't
silently nuke the user's work.

**When to opt into backdrop close:** info modals, simple confirmations,
anything where state preservation isn't a concern and quick dismissal is a UX
win.

**When to opt into `preserveState: true`:** multi-step wizards, long edit forms
where an accidental close-then-reopen shouldn't lose work. It also fits a modal
whose content is rebuilt on every open: the Processes page's sheet scrim
(`hideSheet`) passes it, because the reset would be work done on fields about to
be thrown away.

Most modals — anything with serious input — get the safe defaults
automatically. The combo of "button-only close + clear-on-close" means an
accidental backdrop click does nothing, and a deliberate close starts the next
session fresh.

### Sheets: one shell, one width, one Escape order

A page that opens several dialogs opens them through **one shell**. On the
Edge Processes page that is `openSheet` (`processes-desktop.js`): one
`.pd-modal` card on the `.modal-overlay` scrim, header, body, a footer with the
sheet's status line, Cancel and the action. Advanced and every one-field sheet
use the same card, so the desktop has one dialog and not one per feature.

**One width, derived from the field the sheet holds.** A sheet field is a
230 px label column, a 14 px gap and a 420 px wide input; add 24 px of body
padding and a 1 px border each side and 18 px for the body's scrollbar when a
long sheet scrolls, and every sheet is **732 px**. A width set per sheet sits
under that sum, and the input runs out of the card. Below a 780 px window (the
card plus the scrim's margin) the card is capped at the window and each label
stacks above its input instead of pushing into a horizontal scroll.
`TestDesktopSheetFitsItsWideField` reads the four rules from the stylesheet and
holds the width to their sum, so changing one without the width goes red.

**One Escape order: innermost first.** One key handler (`onEscape`) walks the
layers the page stacks, innermost first, and acts on the first it finds: an
open popover closes — the page's (`#pd-pop`), Settings' group list
(`#pd-stpop`), or a picker a dialog opened over itself (`#pd-advpop`);
else, while the discard question is up, Escape does nothing; else the open
layer — Advanced, then an `openSheet` sheet — asks or closes (below); else, on
the Styles tab, a pinned recipe group and then the selected position are let
go. Closing the sheet under an open menu would leave the menu floating over
nothing.

**A sheet with typed input asks before it discards.** An Escape on a sheet
nobody has typed into closes it. On a sheet with typed work it opens
*Discard the changes?* with **Keep editing**, which puts every field back
exactly as it was, and **Discard**, which closes the layer the way its own
Cancel does. Advanced and every `openSheet` sheet ask the same question, and
while it is up Escape does nothing: only its two buttons answer it. "Typed" is read off the DOM, not kept in a ledger: a field whose
property (`value`, `checked`) differs from the attribute it was drawn with has
been edited. The question is for Escape — a key that elsewhere only closes
menus. Cancel is a deliberate click and discards without
asking; the backdrop does not close a sheet at all (the lifecycle default
above).

### The layer: one stack

`shared/layer.js` is the one overlay stack. Every sheet, modal, confirm,
prompt and popover a page opens is an entry in it, innermost last, and one set
of rules holds for all of them. Both hubs' modal names are the layer's (wave
C). A Core page keeps the state it holds beside a modal in the entry's
`onClose`, which runs however the entry closed, and leaves Escape to the layer
(`TestCorePagesLeaveEscapeToTheLayer`).

| Behaviour | On the layer |
|---|---|
| Escape | Closes the innermost entry, one per press. With nothing open, Escape is the page's: a page's own Escape acts only when `layerDepth()` is 0. |
| Backdrop click | Escape for the innermost entry, where the entry opts in: `data-backdrop-close`, `closeOnBackdrop`, or `backdrop: true`. A press that began inside the card never closes it. |
| Typed work | Escape or the backdrop asks *Discard the changes?* (**Keep editing** / **Discard**); Escape on the question is Keep editing. Cancel and `hideModal` are deliberate and never ask. |
| Scroll lock | Counted: on while any modal, sheet or dialog is open, back to the page's own value when the last one closes. A popover locks nothing. |
| Focus | Moves in on open (the element asked for, `[autofocus]`, the first field, the first button) and back to the opener on close. |
| Fields | Back to their markup on close (a form reset), unless the entry keeps its state (`preserveState`). A hidden input is left alone. |
| Confirm, prompt | Always resolve: OK is `true` or the typed text, anything else that closes them is `false` or `null`. |
| Popover | Closes on Escape, on a press outside it and its anchor, and with the entry it was opened inside. |

**Typed work is what moved since the entry opened.** The layer notes every
field's value when an entry opens; a field drawn later is read against the
value its markup was drawn with (`data-was`, or the `value` / `checked`
attribute). A field marked `data-scratch` (a search box) is never work; an
element marked `data-dirty` (a picker whose selection moved) always is. A page
that fills its fields after opening calls the handle's `markClean()`.

`openLayer(el, opts)` registers an element and returns its handle; `closeLayer`
closes one. The handle's `run(fn)` is one run per press: the entry's
`[data-layer-ok]` buttons are off while it runs and back on when it ends,
whatever the run did. Its `state` lives while the entry is open and is dropped
when it closes, so a chain that refused midway resumes at the step that failed
and nothing outlives the sheet. `status(text, bad)` writes the entry's one line
of feedback (`[data-layer-status]`). `openSheet` is the Processes page's sheet
shell on the layer: title, body, status line, Cancel and OK. Focus goes back to
what had it when the entry opened; `opener` (on `openLayer` and `openSheet`)
names another element, for a sheet opened from a menu row that is gone by the
time it closes: the menu's trigger. `showModal`,
`hideModal`, `confirm`, `prompt` and `confirmWithInput` are the hubs' names on
the same stack.

`TestSharedComponentSpecs` holds each rule; `TestSharedComponentPins` runs
the helper pins' modal and confirm / prompt cases on the layer.

### Touch variant

The Operator HMI uses the same mechanism (`.modal-overlay` + `.active`) with a
`.modal--touch` modifier on the overlay. Its rules in `operator.css` are scoped
to `.modal-overlay.modal--touch`, so they cannot clash with the admin
surfaces' modal rules; touch-target sizing is in [Buttons](12-buttons.md).

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

- `style="display:none"` toggled by JS — fragile, no transitions
- HTML5 `hidden` attribute — inconsistent browser styling
- Per-page modal markup — use the shared structure
- An inline `onclick` to close it — use a `data-action` (see
  [Event handling](28-event-handling-delegated-actions.md))

