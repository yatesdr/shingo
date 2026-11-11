## Code organization

Verified against: 51489c78

### The shared module

`shared/` is its own Go module in the workspace `go.work` declares; the root
[README](../../README.md) lists every module. Core and Edge each require
`shingo/shared` (and `shingo/protocol`) with a `replace` to the sibling
directory, so an edit to `shared/` is in the next build with no version bump.
`shared/` holds what Core and Edge must present or answer identically. It is not
the home for shared infrastructure; that is `protocol/`.

What it holds, by kind:

- **UI assets**, at the top level and embedded by `static.go`
  (`//go:embed *.css *.js *.svg`): `tokens.css` (the design tokens),
  `status-classes.css` (one badge class per protocol status), `components.css`
  (the component rules both apps share), the JS primitives one job per file
  (`dom.js` the HTML escape and DOM helpers, `clock.js` the server's clock and
  the time formatters, `list.js` table sort and keyed lists, `settings.js`
  the settings page, `utils.js` the rest; see
  [JavaScript primitives](17-javascript-primitives.md)), `esc.js` (the HTML
  escape for string-built markup), `scene-geom.js` (the map projection) and
  `icons.svg` (the icon sprite).
- **Go helpers the UI drift tests and both `www` packages call**: chip and
  class-selector reading (`chips.go`, `cssclasses.go`), contrast and
  colour-vision maths (`contrast.go`, `cvd.go`), the emoji detector
  (`emoji.go`), the layer scale (`zlayer.go`), and two HTTP helpers
  (`shared.SetHTMLContentType`, `shared.SafeNextPath`).
- **Cross-surface answers**, one package each: `planttime` (timestamps on the
  plant clock), `clockglobals` (the page globals `clock.js` needs),
  `windoworder` (the order of a loader's windows).
- **Cross-module fixtures** pinning Core against Edge: `loadervectors`,
  `comingvectors`, `fedshape`, `scenefixtures`.
- **Repo tooling** under `scripts/` (Go, not shipped).

The binaries embed the shared assets at build time; nothing reads `shared/` at
run time.

### Static file serving

Each consumer serves `shared.Files` under one prefix (`/static/shared/`):

```go
import "shingo/shared"

r.Handle("/static/shared/*", http.StripPrefix("/static/shared/",
    http.FileServer(http.FS(shared.Files))))
```

Templates reference the assets by that prefix:

```html
<link rel="stylesheet" href="/static/shared/tokens.css">
<script type="module" src="/static/shared/utils.js"></script>
```

### Adding to shared/

Promote a file to `shared/` only when **both** Core and Edge need it
identically, and only when a disagreement between them would be a defect.
Don't populate it ahead of need. The full criterion, and the rule that the drift
guard ships in the promoting commit, is in
[shared-layer-promotion.md](../shared-layer-promotion.md); read it before
promoting anything.
