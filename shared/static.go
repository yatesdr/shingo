// Package shared holds what Core and Edge must present or answer identically.
//
// It began as UI assets — CSS tokens, status badge classes, JS utilities that
// both admin surfaces consume so the two stay structurally aligned — and
// several descriptions still stop there. It also holds cross-surface answers
// (windoworder) and cross-module test fixtures (loadervectors). It is NOT the
// home for shared infrastructure; that is protocol/.
//
// See docs/ui-style-guide.md for the UI half and
// docs/shared-layer-promotion.md for what may be promoted here at all.
//
// Consumers wire the embedded FS into their own HTTP layer at a fixed
// URL prefix:
//
//	import "shingo/shared"
//
//	http.Handle("/static/shared/", http.StripPrefix("/static/shared/",
//	    http.FileServer(http.FS(shared.Files))))
//
// Templates then reference the assets by that prefix:
//
//	<link rel="stylesheet" href="/static/shared/tokens.css">
//	<link rel="stylesheet" href="/static/shared/status-classes.css">
//	<script type="module" src="/static/shared/utils.js"></script>
//
// The embed.FS is rooted at the module directory; the assets sit at the
// top level (tokens.css, status-classes.css, utils.js).
package shared

import (
	"embed"
	"io/fs"
)

//go:embed *.css *.js *.svg
var assets embed.FS

// Files is the read-only filesystem of shared UI assets, suitable for
// http.FileServer(http.FS(Files)).
var Files fs.FS = assets
