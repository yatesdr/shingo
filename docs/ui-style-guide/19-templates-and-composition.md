## Templates and composition

The three surfaces use three different Go template composition models, and
that's fine. Don't migrate.

| Surface | Pattern | Used because |
|---|---|---|
| Core | `{{define "layout"}}` + `{{block "content" .}}` (inheritance) | SSR + client enhancement, single layout |
| Edge admin | `{{template "header" .}}` + `{{template "footer" .}}` (sandwich) | HTMX partial swaps need standalone named templates |
| Operator HMI | Empty-shell HTML + JS render (no Go templates beyond the shell) | Fully client-rendered from JSON, single persistent connection |

### Shared partials

A small `templates/shared/` directory contains primitives that any surface
can `{{template}}` in:

- `status-badge.html` — `{{template "status-badge" .Status}}`
- `fieldset-card.html` — wrap form sections
- `form-field.html` — label + input + error layout

Add to this directory **only when a concrete need surfaces** — a partial that
two or more surfaces need to render identically. Don't speculatively populate.

### Inline scripts

**Don't.** Edge templates currently have significant inline `<script>` blocks
(notably `material.html:39-251`). New code does not add inline scripts;
existing inline scripts are extracted to `static/js/pages/<page>.js` when
the file is touched.

The one allowed inline `<script>` pattern is data-handoff from server to
client, and it should use JSON-in-attribute, not `window.foo = ...`:

```html
<!-- GOOD -->
<div id="page-data" data-claims='{{.ClaimsJSON}}'></div>

<!-- BAD — quote-fragile, no type safety -->
<script>window.claimedByStation = {{json .ClaimedByStation}};</script>
```

The Go handler emits `ClaimsJSON` via `json.Marshal`; the page JS reads
`JSON.parse(document.getElementById('page-data').dataset.claims)`.

