## Deprecations tracker

Scheduled removals live here. The tracker is the block below — there is no
separate deprecations file, and a link to one would be a link to nothing.

```markdown
### The Processes settings-shape names (`.pd-`) — superseded by `.set-` (U1, 2026-10-07)
- **Moved:** the rules now live in `shared/components.css`, each grouped with its
  neutral name (§33 has the map): `.pd-sect`, `.pd-sheet` (+ `.pd-settings`),
  `.pd-sect .pd-spacer`, `.pd-sfld`, `.pd-note` (both definitions), `.pd-savebar`,
  `.pd-btn` (+ `.quiet`, `.primary`, `.danger`), `.pd-inp` (+ `.wide`),
  `.pd-chk`, `.pd-seg`, `.pd-cnt`, `.pd-notice`, `.pd-refusal`, `.pd-dim`, `.pd-chip`
  (the plain chip only).
- **Status:** the Processes page (`processes-desktop.js`) still uses the `.pd-`
  names and is not being renamed; new code uses the `.set-` names only.
- **Off the type scale:** these rules carry their own px sizes and spacing
  (11.5 / 12 / 12.5 / 13 / 16 px type; 300 / 314 / 96 / 420 px geometry), not the
  `--font-*` and `--sp-*` tokens. They were moved verbatim; outside the settings
  shape, do not copy the values into new rules.
```
