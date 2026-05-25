// Package shared exposes UI assets (CSS tokens, status badge classes,
// JS utilities) that Core admin and Edge admin both consume so the two
// surfaces stay structurally aligned. See docs/ui-style-guide.md.
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

//go:embed *.css *.js
var assets embed.FS

// Files is the read-only filesystem of shared UI assets, suitable for
// http.FileServer(http.FS(Files)).
var Files fs.FS = assets
