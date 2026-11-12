package www

import (
	"testing"

	"shingo/shared/assets/assetcheck"
)

// TestTemplates_OnlyTheHandlersHashForm holds every template to the one way
// of naming a static file: a src or href is {{assetURL "/static/…"}} on a file
// the handler serves, which renders the hashed URL cached for good; a module
// entry is an inline import of a plain URL after the page's import map, never
// a src tag; every importMap argument is a plain URL the handler serves.
//
// WHY NOTHING ELSE. Every asset tag used to carry ?v=<per-render stamp>, so
// every stamped asset was downloaded in full on every page view, and a module
// named by a stamped tag and by a bare import was two URLs and two module
// instances. A query, a hand-written hash or a plain URL in a tag is each a
// way back to one of those: a plain URL revalidates on every view, a
// hand-written hash goes stale on the next change, and a src module tag is
// one URL with the map and another without. Read from source, so a tag on a
// branch the render tests never take is held too.
func TestTemplates_OnlyTheHandlersHashForm(t *testing.T) {
	t.Parallel()
	problems, err := assetcheck.Templates(templatesFS, staticAssets(), "templates/*.html", "templates/partials/*.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Error(p)
	}
}
