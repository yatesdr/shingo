## Forms

### Markup

Every form input is wrapped in a `.form-group` with an explicit `<label>`
and the input class:

```html
<div class="form-group">
  <label for="process-name">Name</label>
  <input type="text" id="process-name" class="form-input">
  <div class="form-error" data-error-for="process-name"></div>
</div>
```

The `.form-input` class is **mandatory** on inputs, selects, and textareas.
This is the Edge convention; Core inputs need the class added during
migration.

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
  await api.post('/api/style-node-claims', state);
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

- ❌ Reading element values inside the save function (`document.getElementById('foo').value.trim()` in `save()`)
- ❌ Setting `element.style.display = 'none'` from event handlers
- ❌ Storing state in `data-*` attributes for retrieval later
- ❌ Multiple "reset", "populate", "save" functions that each touch the same 20 IDs

### Worked example

The claim editor that used to be the canonical example here —
`shingo-edge/www/static/js/pages/processes.js` — was **deleted in U9d**, and
the reason it was is worth more than the example was. It demonstrated the
conventions perfectly and was still the wrong shape: one state object, a pure
`claimFieldVisibility(role, swap)` returning element-id → boolean, a pure
validator, single-direction snapshot functions, one DOM-mutation entry point,
and 528 characterization assertions holding all of it. What it could not fix is
that it was **thirty fields on one modal**, and no amount of state discipline
makes thirty fields answerable.

What replaced it keeps every convention and moves the question. The desktop
composer splits those thirty by whether the PICTURE DRAWS THEM: the fields a
picture can show are edited on the picture and in a table under it, and the
twelve it cannot are in one sheet per position that says, in its own footer,
that nothing in it changes the flow. The state object is
`operator-station/composer-model.js` — pure, no DOM, `reduce(state, action)`,
shared unchanged with the operator HMI. The visibility table is gone entirely:
`advancedShows()` asks **flowspec**, which is the same table the server
validates against, so a control cannot be offered for a value the save would
refuse.

Copy from it:

- **One state object, shared between surfaces.** `composer-model.js` is the
  draft for the HMI and the desktop both. Two screens over one model is what
  makes "an engineer and an operator see the same flow" true rather than
  aspirational.
- **The oracle is the server's own table, not a hand-written map.** flowspec is
  exported from Go and embedded as `flowspec-data.js` under a drift test. A
  visibility map maintained by hand is a second opinion about what the server
  accepts, and it is always the stale one.
- **A pure `toCells(state)` is the wire.** The characterization test compares
  its output to bytes generated by Go's own `domain.Collapse`, so a shape change
  on the server fails the JS the same day.
- **Pointer semantics for "no opinion".** A draft that has never been touched
  says nothing about a column, and the server's carry-through keeps what is
  stored. A form that sends every field it knows about flattens the fields it
  does not show.
- **`composer-fields.characterization.test.js`** pins the same (role ×
  swap_mode) matrix the old suite did — 689 assertions, and its runner refuses
  to pass below the 528 the retired suite reported. When a form is replaced,
  port the suite before deleting the code, and make the replacement's floor the
  original's count.

The conventions above are the parts to copy when a new form needs
this treatment. Two-field "save three values" modals don't need the
full machinery — apply the convention when conditional visibility or
multi-step validation enter the picture.

