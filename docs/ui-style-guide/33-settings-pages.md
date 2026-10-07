## Settings pages

A settings page is the shape the Edge Processes page's Settings tab drew first
(reference shot `hmi-flow-composer-design-2026-09-02/desktop/reference/D5-settings.png`),
shared so the Core and Edge Configuration pages are built from it too. The
CSS is one set of rules in `shared/components.css`; the behaviour is
`settingsPage` in `shared/utils.js`. Sizes and colours come from those rules,
not from a mock-up.

### What a settings page looks like

1. **One page, one column, no cards and no tabs.** Sections separated by a
   `--sub-1` rule, capped at the Settings width (1040 px). Every section is
   visible by scrolling; the common ones fit one screen at 900 px.
2. **Live state is words in the section title** (`Fleet · connected`), dot +
   muted text. Never a pill, never beside a switch. Warning hue only for a state
   that needs someone (`no timezone set · showing UTC`).
3. **A row is a question.** Label in plain words that names what is being set;
   one-line sub-label that says what it does or what off/on mean. The sub-label
   also carries `applies after a restart` where true; there is no coloured tag
   for it.
4. **The control is the size of its answer.** A port is 96 px, a host 240, a
   URL 420. Never stretched to the column. Two values that are one answer share a
   row (host + port, user + password, bucket + region).
5. **A duration is one box with its unit in it** (`45 min`, `10 s`, `1 h`).
   Parsed to a Go duration on the wire; a Go string typed in is accepted. Never
   `45m0s` on screen.
6. **Two to four short answers are a segment, not a select.** A switch for
   on/off. No pop-over menu on a settings page.
7. **A row that is off hides what depends on it** (computed from state, §13).
   Rarely changed values sit behind one row that shows their current values in
   its sub-label and a `Change…` button.
8. **A secret is never rendered.** Placeholder says `saved` or `none saved`;
   blank keeps it; a `Remove` control clears it.
9. **A list is rows in the value column** with `Remove` on the row and `Add …`
   after the last.
10. **One Save for the page**, in the foot bar: `No unsaved changes` /
    `Unsaved changes`, `Discard`, `Save` (disabled until dirty). This is the
    stated exception to §11's save-on-change: a settings page drafts and saves
    together, because a half-typed address must never be applied. An action
    that is not a setting (Test connection, Send a test, Refresh, Back up now,
    Change password) is its own button and does not touch the draft.
11. **After a save that needs a restart**, a notice above the bar names the
    fields until the process restarts.

### The classes

Use the neutral names only. The page container is `<div class="set-page
set-ui">`, and **the body of every modal a settings page opens also carries
`set-ui`**: a `showModal` element sits outside the page, and on Core its inputs
would otherwise take the global `input` rule.

| Neutral name | What it is | Old name (Processes only) |
|---|---|---|
| `.set-ui` | the scope; every neutral rule is written under it | — |
| `.set-page` | the page: padding, the 1040 px cap; carries `.set-ui` | `.pd-sheet`, `.pd-sheet.pd-settings` |
| `.set-sect` (`.danger`) | a section title row: `<h2>`, live words, `.set-spacer` | `.pd-sect` |
| `.set-spacer` | pushes what follows to the right in a section title | `.pd-sect .pd-spacer` |
| `.set-fld` | a row: `<label>` (with `<small>` sub-label) + `.v` value box (`.v.col` stacks) | `.pd-sfld` |
| `.set-note` | a note under a row, in the value column | `.pd-note` (both definitions) |
| `.set-err` | the error line under a row (added) | — |
| `.set-inp` | a text box, 96 px floor; `.mid` 240 (added), `.wide` 420 | `.pd-inp`, `.pd-inp.wide` |
| `.set-chk` (`.on`) | the switch | `.pd-chk` |
| `.set-seg` (`button.on`) | a segment of two to four answers | `.pd-seg` |
| `.set-btn` (`.primary`, `.quiet`, `.danger`) | a button | `.pd-btn` |
| `.set-cnt` (`.ok`, `.warn` added) | the live words: `<span class="set-cnt ok"><i></i>connected</span>` | `.pd-cnt` |
| `.set-dim` | muted text | `.pd-dim` |
| `.set-chip` | a chip, e.g. one PLC seen | `.pd-chip` (the plain chip; `.set` / `.add` / `.need` stay Processes-only) |
| `.set-notice` | the restart notice above the bar | `.pd-notice` |
| `.set-refusal` | a refused or half-applied save, above the bar | `.pd-refusal` |
| `.set-savebar` (`.prov`, `.prov.dirty`) | the foot bar | `.pd-savebar` |

The `.pd-` names are the Processes page's and are deprecated for anything new
(§25). The moved rules' type sizes and spacing are their own px values, off the
token scales: they were moved verbatim and are not a pattern to copy.

**Why `.set-ui`.** Core's `style.css` loads after `components.css` and styles
bare `label` and `input[type=text|password|number|date]` at specificity 0,1,1.
Every neutral rule is `.set-ui .set-x` (0,2,0), so it wins. One reset on the
neutral names restores what the moved rules inherit on the Edge and do not
declare: the 14 px base size (`--font-sm`) on `.set-ui`, `width: auto` on
`.set-inp`, normal weight and no bottom margin on `label`, and `[hidden]` hiding
again (an author `display` on `.set-fld` and the rest would otherwise beat the
browser's own `[hidden]` rule, so a row that is off would still draw: rule 7).
Each app keeps its own font family.

**Widths.** `.set-inp` sets a floor (`min-width`), not a width, so an input
takes its intrinsic width when that is larger: give a port box a small `size`
attribute (e.g. `size="5"`) so it sits at the 96 px floor. Below 900 px the row
stacks (label above control), the note and error lines lose their indent, and a
`.wide` box takes the column.

### The behaviour: `settingsPage`

```js
import { settingsPage, apiResult, durationFromText, durationToText, collectList } from '/static/shared/utils.js';

const page = settingsPage(document.getElementById('config'), {
    url: '/api/config',                 // the one save door, PUT
    restart: data.restartPending,       // the server's notice at render
    sections: {
        fleet: {
            read:     () => ({ host: hostBox.value, poll: pollBox.value }),
            write:    (v) => { hostBox.value = v.host; pollBox.value = v.poll; },
            validate: (d) => durationFromText(d.poll) === null ? { poll: 'Not a duration.' } : null,
            body:     (d) => ({ host: d.host, poll: durationFromText(d.poll) }),
        },
        shifts: {                       // its own door: left out of the shared request
            label: 'shifts',
            read, write,
            save: (d) => apiResult('PUT', '/api/shifts', d.rows).then((r) => r.body || { ok: r.ok }),
        },
    },
});
```

- **Dirty** is per section: `read()` against what was last loaded or saved,
  re-checked after every input, change and click inside the root, and on
  `page.update()`.
- **One shared request per save**: `{section: {...}}` for the dirty sections
  that have no `save` of their own, each through its `body()`, sent with
  `apiResult` (it never throws: `{ok, status, body}`). The answer is `{ok:
  false, error, errors: {field: msg}}` or `{ok: true, applied, restart, failed}`.
- **A section with its own `save(d)`** (a Promise of `{ok, errors?, error?}`)
  is saved after the shared request, on its own. Each part stands alone: a
  section is clean again only if its part saved, and a partial outcome is said
  plainly in the refusal box (`Settings saved; shifts failed: …`). Every
  dirty section is validated first; any client error and nothing is sent.
- **Field errors** are drawn as `.set-err` right under the `.set-fld` row that
  holds `[data-field="<field>"]`; an error with no such field goes in the
  refusal box with the top-level `error`. Client `validate()` errors draw the
  same way and send nothing.
- **`restart`** replaces the notice's list; **`failed`** (saved, but did not
  apply) shows in the refusal box. A clean save toasts `Saved`.
- **Discard** calls each dirty section's `write()` with its last saved values.
- The page may draw its own `.set-savebar` (a `.prov` and buttons
  `[data-set="discard"]`, `[data-set="save"]`); otherwise one is built at the end
  of the root.

**Durations.** `durationToText('45m0s')` is `45 min`; `durationFromText('45 min')`
is `45m0s`, and a Go string typed in (`1h30m`) is accepted. Blank is `''`, text
that is not a duration is `null`.

**Lists.** `collectList(container, '.row', read)` reads the rows that are on the
page, in order. Never rebuild a list from indexes: removing row 0 must not lose
the rows after it.
