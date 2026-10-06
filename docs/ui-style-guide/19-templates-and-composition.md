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

### Standalone pages, frames and the nav (Core)

- **A standalone page that fixes its theme says so to the browser.** The wall
  displays hard-code `data-theme="dark"`; without
  `<meta name="color-scheme" content="dark">` the browser draws light
  scrollbars and controls on them — a white bar down a display framed on the
  Dashboard. Guard: `TestKioskTemplatesDeclareTheirColorScheme`.
- **A framed page fills what the chrome leaves, by layout.** The wall-display
  frame is a flex column the height of the viewport (`.wall-frame`), not
  `calc(100vh - <pixels>)` — a pixel constant is wrong the moment the chrome
  grows (the sim strip), and the frame is then cut off behind a second
  scrollbar.
- **The nav shows a logged-out reader only links it can open.** A link that
  answers with the login page is wrapped in `{{if .Authenticated}}` in
  `layout.html`. The router's `requireAuth` group is the authority, and
  `TestNav_LoggedOutLinksOpenWithoutLogin` asks the real router about every
  logged-out nav link, so a page moved behind the login without its link fails
  there.
- **The login page never prints credentials.**

