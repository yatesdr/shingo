package www

import "net/http"

// staticCache makes a served asset go stale exactly when the binary changes, and
// never otherwise.
//
// WHY IT EXISTS. Static assets were served with no validator at all — an
// embed.FS reports a zero modtime, so http.ServeContent emits no Last-Modified,
// and nothing emitted an ETag. Freshness was bought instead by hanging
// ?v={{cacheBust}} on each <script> tag, where cacheBust is a fresh nanosecond
// timestamp PER CALL. That worked, at the cost of refetching every asset on
// every page load, and it only covered assets a TEMPLATE names: a module reached
// by a bare `import` (shared/utils.js, components/*, pages/overview/*,
// nodes-detail.js, nodes-maintain.js) carried no query and had no validator
// either, so it was cached heuristically and could go stale after a deploy with
// nothing to correct it.
//
// It also made a URL-identity trap. A module reached BOTH by a busted tag and by
// a bare import is two URLs, so the browser instantiates it twice, with two
// copies of its module state — and after an upgrade the two copies can be
// different VERSIONS of the file. That is the bug MG1C-1 fixed for
// nodes-detail.js and MG1C-6 fixes for app.js, and the query is what made it
// possible.
//
// no-cache DOES NOT MEAN "do not cache". It means "cache it, but revalidate
// before reusing it" — so the browser keeps the file and sends a conditional
// request, which this answers with 304 and no body for as long as the process
// lives. Assets are re-downloaded on a restart and not before, which is what the
// cache-bust was approximating.
//
// THE ETAG IS THE PROCESS IDENTITY, deliberately, rather than a per-file hash.
// It is the same serverInstance the SSE `connected` event carries to trigger a
// tab reload on redeploy, so the two freshness mechanisms cannot disagree about
// what "a new build" means. The cost is that a restart invalidates every asset
// rather than the ones that changed; for a handful of files on a plant LAN that
// is not worth a content hash and a build step.
//
// Set BEFORE the FileServer runs: http.ServeContent reads the ETag already
// present on the response header and does the If-None-Match comparison itself,
// so this middleware gets correct 304s without reimplementing them.
func staticCache(next http.Handler) http.Handler {
	etag := `"` + serverInstance + `"`
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}
