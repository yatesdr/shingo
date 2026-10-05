## Dialog UX — confirmation, prompt, toast

### Never use native dialogs

`alert()`, `confirm()`, `prompt()` are forbidden. Use the shared helpers.

```js
import { confirm, toast, prompt } from '/static/shared/dialog.js';

// Confirmation — Promise-based, styled overlay
if (!await confirm('Delete this style?')) return;

// Toast — auto-dismissing notification
toast('Saved', 'success');
toast('Network error', 'error', { sticky: true });

// Prompt — styled input dialog, not native
const count = await prompt('Remaining parts?', { type: 'number', min: 0 });
if (count === null) return;
```

### Migration rule

When touching a file with `confirm()` / `alert()` / `prompt()`, migrate them
in the same PR. The migration is mechanical (`if (!confirm(...))` →
`if (!await confirm(...))`), but every call site needs to be in an `async`
context — verify the enclosing function is `async` or refactor.

### Toast levels

| Level | Use for | Default duration |
|---|---|---|
| `success` | Mutation succeeded | 3.2s |
| `error` | Mutation failed, network error | sticky if `{ sticky: true }`, else 5s |
| `warning` | Validation failure, soft block | 3.2s |
| `info` | Background event the user should know about | 3.2s |

Sticky errors are the default for async/SSE-delivered failures (operator
might have looked away).

