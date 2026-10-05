## Tabs

One implementation. CSS:

```css
.tabs            { display: flex; gap: 0.25rem; border-bottom: 1px solid var(--border); }
.tab             { padding: 0.5rem 1rem; cursor: pointer; border: none; background: none; color: var(--text-muted); }
.tab:hover       { color: var(--text); }
.tab.active      { color: var(--primary); border-bottom: 2px solid var(--primary); margin-bottom: -1px; }
.tab-panel       { display: none; }
.tab-panel.active { display: block; }
```

Markup:

```html
<div class="tabs">
  <button class="tab active" data-tab="general">General</button>
  <button class="tab" data-tab="claims">Node Claims</button>
  <button class="tab" data-tab="stations">Operator Screens</button>
</div>
<div class="tab-panel active" id="tab-general">...</div>
<div class="tab-panel" id="tab-claims">...</div>
<div class="tab-panel" id="tab-stations">...</div>
```

JS handler is shared. No more `.tab-bar` / `.diag-tabs` / `.spot-tabs` /
`.to-tabs` / `.process-tab`.

### Tabs are not CTAs

Don't style tabs as `.btn-primary` with `.active`. Tabs are navigation;
primary buttons are actions. The `.tabs` styling above keeps them
visually distinct.

