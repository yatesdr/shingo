package www

import (
	"io/fs"
	"strings"
	"testing"
)

// TestNoCoreFileImportsUtilsJS: Core's browser code takes every shared helper
// from the one-job file that defines it (shared/dom.js, clock.js, list.js,
// bus.js, settings.js, layer.js, notice.js, http.js) and its api verbs from
// app.js. shared/utils.js is what is left of the old all-in-one file, kept
// until its last importer goes; Core is not one, so a page that names it again
// fails here rather than keeping the remnants alive.
func TestNoCoreFileImportsUtilsJS(t *testing.T) {
	var bad []string
	for path, src := range pageJS(t) {
		if strings.Contains(stripLineComments(src), "/static/shared/utils.js") {
			bad = append(bad, path)
		}
	}
	err := fs.WalkDir(templateFS, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".html") {
			return err
		}
		b, err := fs.ReadFile(templateFS, path)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), "/static/shared/utils.js") {
			bad = append(bad, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk templates: %v", err)
	}
	for _, p := range bad {
		t.Errorf("%s names /static/shared/utils.js: import the helper from the shared file that defines it", p)
	}
}
