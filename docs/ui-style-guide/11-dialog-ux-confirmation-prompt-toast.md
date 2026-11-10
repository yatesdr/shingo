## Dialog UX — confirmation, prompt, toast

Verified against: 51489c78

### Never use native dialogs

`alert()`, `confirm()`, `prompt()` are forbidden. Use the shared helpers in
`shared/utils.js`; each `confirm` / `prompt` call site is `await`ed, so its
enclosing function is `async`.

```js
import { confirm, toast, prompt } from '/static/shared/utils.js';

// Confirmation — Promise-based, styled overlay
if (!await confirm('Delete this style?')) return;

// Toast — auto-dismissing notification
toast('Saved', 'success');
toast('Network error', 'error', { sticky: true });

// Prompt — styled input dialog, not native
const count = await prompt('Remaining parts?', { type: 'number', min: 0 });
if (count === null) return;
```

### Toast levels

| Level | Use for | Default duration |
|---|---|---|
| `success` | Mutation succeeded | 3.2s |
| `error` | Mutation failed, network error | sticky if `{ sticky: true }`, else 5s |
| `warning` | Validation failure, soft block | 3.2s |
| `info` | Background event the user should know about | 3.2s |

Make an async or SSE-delivered failure sticky: the operator might have looked
away.

### Save on change, ask before losing data

**One rule for every page.** An edit saves the moment it is made: a select
commits on `change` and a checkbox when it is ticked, with no separate Save
button for a single field. Core asks first (`uiConfirm`) only when the change
**deletes something or loses data**: removing a member that carries a payload
or threshold, a layout switch that drops members, Delete, Cancel, Terminate.
An edit that loses nothing does not ask. A confirmation on every change
teaches people to click through the one that matters.

**The exception: a settings page.** A settings page (see
[Settings pages](33-settings-pages.md): Processes Settings, Core and Edge
Configuration) drafts and saves together, with one Save in its foot bar,
because a half-typed address must never be applied.

The station box's settings panel (*Settings that save as ticked*, in
[Placement boxes](14-placement-boxes-the-box-is-the-form.md)) is this rule on a
box. The Nodes page follows it: a dedicated position's payload `change` is
instant, and its `×` asks only when the position has a payload or threshold
set. A destructive control on every row (Cancel on an active order, Delete on a
payload) keeps its confirmation and loses the solid red. Weight goes to the
action a row is for, not to the one that ends it.
