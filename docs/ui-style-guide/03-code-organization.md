## Code organization

### Shared module structure

**Decided: its own Go module + Go workspace.**

`shared/` was the third module when this section was written and the UI assets
below were all of it. The workspace now lists five (`protocol`, `shared`,
`shingo-core`, `shingo-edge`, `integration`), and `shared/` has since taken on
cross-surface answers and cross-module test fixtures alongside the assets. It is
still not the home for shared infrastructure — that is `protocol/`. See
[`shared-layer-promotion.md`](../shared-layer-promotion.md).

```
shingo/                          ← repo root
├── go.work                      ← workspace file, lists all five modules
├── protocol/
│   └── go.mod                   ← wire protocol + shared infrastructure
├── shingo-core/
│   └── go.mod                   ← imports shingo/shared
├── shingo-edge/
│   └── go.mod                   ← imports shingo/shared
└── shared/
    ├── go.mod
    ├── static.go                ← go:embed *.css *.js *.html
    ├── tokens.css               ← semantic design tokens
    ├── status-classes.css       ← per-status badge classes
    ├── utils.js                 ← h, el, escapeHtml, api, modal, confirm, toast, SSE factory
    ├── windoworder/             ← a cross-surface answer, not an asset
    └── loadervectors/           ← cross-module fixtures pinning Core against Edge
```

The `go.work` file at the repo root declares all of them as a
workspace. Local development picks up edits to `shared/` immediately; no
version bumps or `replace` directives needed during normal work. Plant
deploys (`git pull` + service restart + rebuild) work transparently — the
workspace file is detected automatically by every `go` command. The
self-contained Go binary embeds the shared static files at build time;
there's no runtime dependency on the `shared/` directory.

### Static file serving

Each consumer module imports `shingo/shared` and serves its files at a
predictable URL prefix (e.g. `/static/shared/utils.js`,
`/static/shared/tokens.css`). The Go side wires this up via:

```go
import "shingo/shared"

http.Handle("/static/shared/", http.StripPrefix("/static/shared/",
    http.FileServer(http.FS(shared.Files))))
```

Template references use the prefixed path:

```html
<link rel="stylesheet" href="/static/shared/tokens.css">
<script type="module" src="/static/shared/utils.js"></script>
```

### Adding to shared/

Promote a file to `shared/` only when **both** Core and Edge need it
identically, and only when a disagreement between them would actually be a
defect. Don't preemptively populate. The full criterion — all four clauses, and
the rule that the drift guard ships in the promoting commit — is in
[`docs/shared-layer-promotion.md`](../shared-layer-promotion.md); read it before
promoting anything.

For UI specifically the candidates are tokens, status-classes CSS and the JS
utility module. Note that `shared/` is no longer UI-only: it also holds
cross-surface answers (`windoworder`) and cross-module fixtures
(`loadervectors`). Shared *infrastructure* does not go here — that is
`protocol/`.

