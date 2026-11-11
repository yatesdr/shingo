package www

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// WHERE THE NODE TESTS LIVE. A *.test.js that pins page code sits in
// testdata/static/, a mirror of static/, never beside its page: //go:embed
// static/* takes every file under static/ into the binary, the static handler
// serves the whole tree, and go:embed has no exclude pattern. A test beside
// its page ships in every Core and answers at /static/<path>. The two pins
// below hold that line: one reads what the binary carries, the other the tree
// a new test would be committed into.

// TestEmbeddedStatic_HoldsNoTestJS walks the embedded static tree and fails
// on any *.test.js in it. The static handler serves only what is embedded, so
// a file the walk does not find is a URL no page and no request can reach.
func TestEmbeddedStatic_HoldsNoTestJS(t *testing.T) {
	t.Parallel()
	files := 0
	err := fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		files++
		if strings.HasSuffix(p, ".test.js") {
			t.Errorf("the binary embeds and serves %s, a node test: move it to testdata/%s", p, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the embedded static tree: %v", err)
	}
	if files == 0 {
		t.Fatal("the embedded static tree is empty, so the walk proved nothing")
	}
}

// TestStaticTree_HoldsNoTestJS walks static/ on disk, so a new test committed
// beside its page fails here, naming where it goes, whatever the embed
// pattern becomes. The tests' own tree must still hold them: an empty
// testdata/static/ means the wrappers lost their files, not that the rule held.
func TestStaticTree_HoldsNoTestJS(t *testing.T) {
	t.Parallel()
	err := filepath.WalkDir("static", func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".test.js") {
			t.Errorf("%s is under the embedded static tree: move it to %s", filepath.ToSlash(p), filepath.ToSlash(filepath.Join("testdata", p)))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk static/: %v", err)
	}
	tests := 0
	err = filepath.WalkDir(filepath.Join("testdata", "static"), func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".test.js") {
			tests++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk testdata/static/: %v", err)
	}
	if tests == 0 {
		t.Error("testdata/static/ holds no *.test.js: the node tests are not where their wrappers run them")
	}
}
