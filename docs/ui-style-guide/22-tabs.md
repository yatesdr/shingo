## Tabs

Verified against: 2cbdbd5b

One implementation. Every tab bar in Core and the Edge is `.tabs` / `.tab`,
with `.tab-panel` siblings, defined once in `shared/components.css`; no page
sets a tab's size. The handler is `switchTab` in `shared/utils.js`, bound as a
delegated action: it marks active the tab whose `data-tab` is the key, and shows
the sibling panel whose id is `tab-<key>`.

Markup:

```html
<div class="tabs">
  <button type="button" class="tab active" data-tab="robots" data-action="switchTab:robots">Robots</button>
  <button type="button" class="tab" data-tab="fleet" data-action="switchTab:fleet">Fleet</button>
</div>
<div class="tab-panel active" id="tab-robots">...</div>
<div class="tab-panel" id="tab-fleet">...</div>
```

A page that loads a panel when it is shown wraps the shared handler rather than
copying it: the Robots page imports it as `showTab` and its own `switchTab`
calls it, then loads the Fleet panel's reports.

A tab that changes page rather than panel is a link, `<a class="tab" href="...">`, with the current one
`aria-current="page"` (`/inventory` and its Count history).

### Tabs are not CTAs

Don't style tabs as `.btn-primary` with `.active`. Tabs are navigation;
primary buttons are actions. The `.tabs` styling keeps them visually distinct.
