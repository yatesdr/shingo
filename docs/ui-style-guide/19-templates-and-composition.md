## Templates and composition

Verified against: 51489c78

The three surfaces use three different Go template composition models, and
that's fine. Don't migrate.

| Surface | Pattern | Used because |
|---|---|---|
| Core | `{{define "layout"}}` + `{{block "content" .}}` (inheritance) | SSR + client enhancement, single layout |
| Edge admin | `{{template "header" .}}` + `{{template "footer" .}}` (sandwich) | HTMX partial swaps need standalone named templates |
| Operator HMI | Empty-shell HTML + JS render (no Go templates beyond the shell) | Fully client-rendered from JSON, single persistent connection |

### Partials

Each module keeps its partials in its own `templates/partials/`, embedded and
parsed with its templates; there is no template directory the two modules
share. A partial both render the same way is written once per module and held
together by a test: `clock-globals.html` (the globals `clock.js` reads) is
audited for both trees by `shared/clockglobals`. Add a partial when a fragment
repeats within a module; don't populate ahead of need.

### Inline scripts

**Don't.** Page JS lives in the module's page scripts (`static/pages/<page>.js`
on Core, `static/js/pages/<page>.js` on Edge), loaded as modules. Two inline
scripts are by design, both because they must run before any module does: the
theme pre-paint in each layout's head, and the clock globals partial
(`window.PLANT_TZ`, `window.SHINGO_CLOCK`), which `clock.js` reads
synchronously.

Data handed from the server to a page goes in a JSON attribute, not a
`window.foo = ...` script:

```html
<!-- GOOD -->
<div id="page-data" data-orders='{{.OrdersJSON}}'></div>

<!-- BAD — quote-fragile, no type safety -->
<script>window.orders = {{.Orders}};</script>
```

The Go handler emits `OrdersJSON` via `json.Marshal`; the page JS reads
`JSON.parse(document.getElementById('page-data').dataset.orders)`. Edge's
Manual message page is the worked example (`handlers_manual_message.go`,
`manual-message.js`).

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

