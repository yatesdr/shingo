## Picker

Verified against: 69ffb314

`shared/picker.js` is the picker's shell: a search box, the rows it filters,
the selection as chips, and an **Add and use** slot. It knows no node and no
part. An adapter hands it rows and decides what each row may be; the shell
draws them and moves the selection. It is the Processes page's node picker,
extracted; the node and part adapters, and the pages, come onto it in S3.

### A row

`{ value, label, section, note, reason, members, values, search }`, every
field but `value` optional:

- `section` is a heading over each run of rows that share one.
- `note` is a quiet line on a row that can be picked.
- `reason` is why a row **cannot** be picked. The row is drawn greyed with the
  reason beside it, and neither a click nor the keyboard reaches it. A picker
  shows what it excludes and why; it never drops a row in silence.
- `members` are rows drawn indented under this one (a group's nodes).
- `values` is what picking the row picks when that is not its own `value` (a
  group standing for its members). The row is on when all of them are.
- `search` is more text the search box matches, beside the label and value.

### Behaviour

- **The search box is never redrawn**: a keystroke redraws the rows and
  chips, never the field under the caret. A member that matches brings its
  group; a group that matches brings all its members.
- **Keyboard.** The arrows move through the rows that can be picked, Enter
  picks, and the box names the active row for a screen reader.
- **Single or multi.** A single picker's pick replaces its value and closes
  its list. A multi-select (`multi: true`) toggles, keeps its list up, and its
  chips are the selection, each with a × that takes it out. The positions
  editor is the picker in multi mode, held nodes greyed with their reason (R4).
- **One list open at a time.** A dropdown's list is a popover on the layer
  (§10), so Escape and a press outside close it before the sheet under it,
  and opening one picker's list closes another's. A picker made with
  `open: true` (a checklist) keeps its list up and is not a layer.
- **Add and use.** Given `add: { run, label }`, a search that names no row
  offers to make it. `run(q)` makes it and resolves its value or row, once per
  press; the shell then picks it. A run that made nothing (the HTTP door
  already said why) changes nothing.
- **Work.** The search box is `data-scratch`, never work. A selection that
  moved marks the picker `data-dirty`, which a sheet asks before discarding;
  `set(values, { clean: true })` and `markClean()` set the starting point.
- **Write-through.** Given `onPick(next, row, picker)`, a pick (a row, a
  chip's ×) is the caller's write. The picked row shows pending and the chips
  do not move until what `onPick` returns, or its promise resolves to,
  answers: the selection as the server now holds it (an array, or
  `{ values }`), drawn as clean; or a refusal (`{ error }`, or nothing), and
  the chips stay as they were with the reason beside them. One pick at a time;
  no timer. A list whose truth is the server (a routing set) uses it, so the
  page never shows a pick the Edge has not taken.

```js
import { createPicker } from '/static/shared/picker.js';

const p = createPicker(host, {
    rows: () => adapter.rows(),        // null until the list is read
    multi: true, open: true,
    filters: [{ label: 'PLN', query: 'PLN_' }],
    onChange: (values) => draft.positions = values,
});
p.refresh();                           // after the adapter's list lands
```

`TestSharedComponentSpecs` holds each rule.
