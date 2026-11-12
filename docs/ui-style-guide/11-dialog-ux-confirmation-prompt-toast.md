## Dialog UX — confirmation, prompt, toast

Verified against: 51489c78

### Never use native dialogs

`alert()`, `confirm()`, `prompt()` are forbidden. Use the shared helpers in
`shared/layer.js` and `shared/notice.js`; each `confirm` / `prompt` call site
is `await`ed, so its enclosing function is `async`.

```js
import { confirm, prompt } from '/static/shared/layer.js';
import { toast } from '/static/shared/notice.js';

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

### The notice: one toast, one banner

`shared/notice.js` draws both into one fixed region at the top of the page:
the banners across it, the toasts stacked on the right below them, so a toast
never covers a banner's button. Pages move onto it in wave C.

- **`toast(message, level, opts)`** keeps the lifetimes above: an error 5 s,
  every other level 3.2 s, `{ sticky: true }` until it is clicked.
  `{ sticky: false }` is the level's own lifetime. The same words at the same
  level already up are one toast, its lifetime started again, so a read that
  fails every ten seconds says so once. A message is text, never markup; an
  Error shows its message. `opts.action` is one control: `{ label, href }` (the
  HTTP door's **Sign in**) or `{ label, onClick }`.
- **`banner(key, opts)`** is a line that stays until it is cleared or its ×
  is pressed: one per key, redrawn in place when shown again. It carries no
  words of its own.
- **The two banners every surface shows** are worded in `notice.js`, once,
  and both can show at once. Core's `app.js`, the Edge hub and the station
  call them on the bus's own frames, so they ride a stream the page already
  holds and open none:

```js
import { onSSE, sseStatus } from '/static/shared/bus.js';
import { clearLostConnectionBanner, showLostConnectionBanner, showNewBuildBanner }
    from '/static/shared/notice.js';

// "A new version is available" + Refresh. Admin pages only (ruling 4: the
// station and Core's dashboards reload instead).
onSSE('build-changed', showNewBuildBanner);

// "Lost connection to Core · last answered 14:02" ('edge': "to this Edge"),
// the plant time of the stream's last frame. No ×: the next connect clears it.
onSSE('disconnected', () => showLostConnectionBanner('core', sseStatus().lastFrameAt));
onSSE('connected', clearLostConnectionBanner);
```

The bus says disconnected again on every reconnect attempt that fails, and
each redraws the words, so the banner arms no timer. A last frame 12 hours or
more ago shows its date too.

`TestSharedComponentSpecs` holds both; `TestSharedComponentPins` runs the
helper pins' toast cases on the notice; `sse.core-hub.txt` and
`sse.edge-admin.txt` show them on a page.

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
