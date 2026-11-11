package www

import (
	"sync"

	"github.com/go-chi/chi/v5"

	"shingo/shared"
	"shingo/shared/assets"
)

// staticAssets is the one handler for every embedded static file: /static/ on
// this binary's tree, /static/shared/ on the shared module's. Core builds the
// same one over its own tree (shingo-core/www/static_assets.go), so a shared
// file answers alike on both.
//
// Built on first use, which is NewRouter's at start-up, and kept for the
// process: the embed cannot change under it. Its ETags are the files' content,
// so a restart that changed no file costs a kiosk 304s, not the page set; the
// per-process serverInstance (sse.go) is only the SSE build id now, and the
// build-change reload is unchanged.
//
// New fails only on a file named like another's hashed URL, a tree mistake
// every www test that builds the router finds before a deploy does.
var staticAssets = sync.OnceValue(func() *assets.Handler {
	h, err := assets.New(
		assets.Mount{Prefix: "/static/", FS: StaticFS()},
		assets.Mount{Prefix: "/static/shared/", FS: shared.Files},
	)
	if err != nil {
		panic(err)
	}
	return h
})

// registerStatic routes the static files to staticAssets. /favicon.ico is the
// browser's root request for /static/favicon.ico, the same file under the
// same ETag. A register helper on Handlers, though it reads nothing of h, so
// the generated route table (shared/scripts/docgen, which walks NewRouter and
// the h.register* helpers it calls) still lists these routes.
func (h *Handlers) registerStatic(r chi.Router) {
	r.Handle("/favicon.ico", staticAssets().File("/static/favicon.ico"))
	r.Handle("/static/*", staticAssets())
}
