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

