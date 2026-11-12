package www

import (
	"io/fs"
	"strings"
	"testing"
)

// TestNoEdgeFileImportsUtilsJS is the Edge twin of Core's
// TestNoCoreFileImportsUtilsJS: the Edge's browser code (admin pages, the
// hub, the operator station) and its templates take every shared helper from
// the one-job file that defines it. shared/utils.js, the old all-in-one file,
// is deleted; a page or template that names it again fails here, not at its
// first load on a plant floor.
func TestNoEdgeFileImportsUtilsJS(t *testing.T) {
	var bad []string
	for _, tree := range []struct {
		fsys fs.FS
		dir  string
		ext  []string
	}{
		{staticFS, "static", []string{".js", ".html"}},
		{templatesFS, "templates", []string{".html"}},
	} {
		err := fs.WalkDir(tree.fsys, tree.dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			want := false
			for _, e := range tree.ext {
				want = want || strings.HasSuffix(path, e)
			}
			if !want {
				return nil
			}
			b, err := fs.ReadFile(tree.fsys, path)
			if err != nil {
				return err
			}
			if strings.Contains(string(b), "/static/shared/utils.js") {
				bad = append(bad, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", tree.dir, err)
		}
	}
	for _, p := range bad {
		t.Errorf("%s names /static/shared/utils.js: import the helper from the shared file that defines it", p)
	}
}
