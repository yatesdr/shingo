## Forms

Verified against: 2cbdbd5b

### Markup

Every form input is wrapped in a `.form-group` with an explicit `<label>`:

```html
<div class="form-group">
  <label for="process-name">Name</label>
  <input type="text" id="process-name" class="form-input">
  <div class="form-error" data-error-for="process-name"></div>
</div>
```

Both apps style an input inside a `.form-group` without a class of its own:
Edge's `shingoedge.css` styles `.form-group input` and `.form-group select`,
and Core's `style.css` styles bare text, number, password and date inputs,
selects and textareas. `.form-input` carries the same rule in both, for an
input that sits outside a group.

**The exception: a settings page** (see [Settings pages](33-settings-pages.md))
uses the settings shape (`.set-fld`, `.set-inp`, …) instead of `.form-group` /
`.form-input`, and drafts and saves together with one Save, because a
half-typed address must never be applied.

### Choice buttons

A question with two or three answers, where the answer decides what else is
asked, is a row of buttons rather than a `<select>`: every answer is visible,
each can carry a one-line sub-label, and the tap target is the whole answer.

```html
<div class="form-group" role="group" aria-labelledby="q-role">
  <div class="station-question" id="q-role">What does it do?</div>
  <div class="station-choice-row">
    <button type="button" class="station-choice" aria-pressed="false" data-action="pickStationRole:produce">Fills bins</button>
    <button type="button" class="station-choice" aria-pressed="false" data-action="pickStationRole:consume">Empties bins</button>
  </div>
  <div class="form-error" data-error-for="q-role"></div>
</div>
```

The pick lives in the form state, not in the button; `render(state)` sets
`aria-pressed` and `.is-selected`. Nothing is pre-picked when the answer changes
what the thing IS — a default there is a guess the person has to notice and
undo. Worked example: the Nodes page's New station card
(`shingo-core/www/static/pages/loaders.js`, `pickStationRole`).

### Form-state convention

Non-trivial forms (modals with conditional fields, multi-step flows,
anything more complex than a save-three-fields modal) follow this pattern:

```js
// state lives in one place
let formState = {
  name: '',
  role: 'consume',
  swapMode: 'single_robot',
  // ...
};

// render(state) → builds/updates the form from state
function render(state) {
  document.getElementById('form-name').value = state.name;
  document.getElementById('form-role').value = state.role;
  // visibility derived from state, not toggled imperatively
  document.getElementById('staging-fieldset').classList.toggle(
    'is-hidden',
    !needsStaging(state.role, state.swapMode)
  );
}

// readFromForm() → snapshots current input values into state
function readFromForm() {
  return {
    name: document.getElementById('form-name').value.trim(),
    role: document.getElementById('form-role').value,
    swapMode: document.getElementById('form-swap-mode').value,
  };
}

// validate(state) → returns { ok, errors }
function validate(state) {
  const errors = [];
  if (!state.name) errors.push({ field: 'name', msg: 'Required' });
  if (state.swapMode === 'two_robot_press_index' && !state.pairedNode) {
    errors.push({ field: 'pairedNode', msg: 'Back Press Node required' });
  }
  return { ok: errors.length === 0, errors };
}

// save(state) → calls the API
async function save(state) {
  const v = validate(state);
  if (!v.ok) { showErrors(v.errors); return; }
  await api.post(saveURL, toWire(state));
}
```

### Rules

1. **State lives in one object.** Not 30 `getElementById` calls scattered
   across 5 functions.
2. **Conditional visibility is computed from state.** Not toggled by
   imperative event handlers.
3. **`validate(state)` is a pure function** — same input, same output, no
   DOM reads. This lets it be unit-tested.
4. **Backend mirrors frontend validation.** Frontend validation is for UX
   (immediate feedback); backend is for correctness. They check the same
   rules.

### Anti-patterns to avoid

- Reading element values inside the save function (`document.getElementById('foo').value.trim()` in `save()`)
- Setting `element.style.display = 'none'` from event handlers
- Storing state in `data-*` attributes for retrieval later
- Multiple "reset", "populate", "save" functions that each touch the same 20 IDs

### Worked example: the flow composer

The flow composer (the desktop Processes page and the station's changeover
screens) is the convention at full size. Its state object is
`shingo-edge/www/static/operator-station/composer-model.js` — pure, no DOM,
`reduce(state, action)` — and the fields it cannot draw on the picture are in
one Advanced sheet per position, whose `advancedShows()` asks **flowspec**: the
same table the server validates against, so a control cannot be offered for a
value the save would refuse. Copy from it:

- **One state object, shared between surfaces.** `composer-model.js` is the
  draft for the HMI and the desktop both. Two screens over one model is what
  makes "an engineer and an operator see the same flow" true rather than
  aspirational.
- **The oracle is the server's own table, not a hand-written map.** flowspec is
  exported from Go and embedded as `flowspec-data.js`, held to the Go export by
  `TestStationFlowspecFileMatchesGo`. A visibility map maintained by hand is a
  second opinion about what the server accepts, and it is always the stale one.
- **A pure `toCells(state)` is the wire.** The characterization test compares
  its output to bytes generated by Go's own `domain.Collapse`, so a shape change
  on the server fails the JS the same day.
- **Pointer semantics for "no opinion".** A draft that has never been touched
  says nothing about a column, and the server's carry-through keeps what is
  stored. A form that sends every field it knows about flattens the fields it
  does not show.
- **A replacement checks at least what it replaced.**
  `shingo-edge/www/static/js/pages/composer-fields.characterization.test.js`
  (run by `TestComposerFieldsCharacterization`) walks the whole (role ×
  swap_mode) matrix, and its runner refuses to pass below
  `RETIRED_SUITE_ASSERTIONS`, the count of the suite it replaced. When a form is
  replaced, port the suite before deleting the code, and make the replacement's
  floor the original's count.

The conventions above are the parts to copy when a new form needs
this treatment. Two-field "save three values" modals don't need the
full machinery — apply the convention when conditional visibility or
multi-step validation enter the picture.

