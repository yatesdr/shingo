## Event handling

### Delegation over inline onclick

```html
<!-- GOOD -->
<button class="btn" data-action="delete-style" data-style-id="42">Delete</button>

<script>
  document.addEventListener('click', (e) => {
    const btn = e.target.closest('[data-action]');
    if (!btn) return;
    if (btn.dataset.action === 'delete-style') {
      deleteStyle(parseInt(btn.dataset.styleId, 10));
    }
  });
</script>
```

```html
<!-- BAD — forces deleteStyle to be a window-global -->
<button onclick="deleteStyle(42)">Delete</button>
```

Inline `onclick=` is forbidden for new code. Reasons:

1. Handler functions must be `window`-global to be reachable, blocking ES
   module adoption.
2. The handler isn't visible at the JS module level (grep finds the HTML
   call, not the binding).
3. CSP-friendly code disallows inline event handlers.

Existing inline `onclick` handlers in `processes.html` and elsewhere are
migrated as part of the rewrite.

### Async handlers

Event handlers that await something must be `async`:

```js
list.addEventListener('click', async (e) => {
  const btn = e.target.closest('[data-action="delete"]');
  if (!btn) return;
  if (!await confirm('Sure?')) return;
  await api.delete('/api/items/' + btn.dataset.id);
});
```

### Pinning a click

**A fault reachable by a click is pinned by a click.** The test dispatches a
real bubbling event at the element the user clicks and lets it travel through
the page's own listeners — the delegated handler on the page root, the
document-level closers, every `stopPropagation` on the way. It never calls the
handler function directly.

The reason is where these faults live. "I click it and nothing happens" is
almost never the handler's body; it is the path to it — an act missing from
the set that stops the event, so the document's closer hides the popover one
listener after it opened; a listener bound to an element a redraw replaced; a
`closest()` selector that no longer matches the markup. A test that calls
`openUsePreset()` passes through every one of those while the click does nothing.
`processes-desktop.popover-acts.test.js` is the pattern: the page module booted
for real, its own `onClick` on `#pd-root` and its own document listener, and a
click walked up the ancestor chain honouring `stopPropagation`. Stub what the
path cannot see (the HTML parser, `fetch`), never the path.
