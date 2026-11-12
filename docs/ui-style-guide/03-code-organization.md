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
  the settings page, `bus.js` the live bus, `layer.js` the overlay stack,
  `notice.js` the toast and banner, `http.js` the HTTP door, `picker.js` the
  picker's shell; see
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

Both routers serve every static file from one handler, `shared/assets`
(`staticAssets` in each binary's `www/static_assets.go`), built once at
start-up: the binary's tree under `/static/` and `shared.Files` under
`/static/shared/`. Each file answers at its plain URL, revalidated by ETag,
and at its hashed URL, cached for good.

Templates name a stylesheet, icon or classic script by its hashed URL, and
reach a module through the page's import map:

```html
<link rel="stylesheet" href="{{assetURL "/static/shared/tokens.css"}}">
{{importMap "/static/app.js" "/static/pages/orders.js"}}
<script type="module">import "/static/pages/orders.js";</script>
```

The import map comes before the page's first module script and names every
entry; it maps each module the page can reach, plain URL to hashed. An entry
is an inline import of the plain URL, never a `src` tag, so a module is one
URL whether the browser reads import maps or not. Core's pages fill the
layout's `importmap` block; an Edge page ends with its map, the hub and its
own entry. `static_asset_query_guard_test.go` and `page_static_set_test.go`
in each binary hold every template to this.

### Adding to shared/

Promote a file to `shared/` only when **both** Core and Edge need it
identically, and only when a disagreement between them would be a defect.
Don't populate it ahead of need. The full criterion, and the rule that the drift
guard ships in the promoting commit, is in
[shared-layer-promotion.md](../shared-layer-promotion.md); read it before
promoting anything.
