package shared

import (
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestSharedFilesEmbedded asserts the foundational embed contract:
// tokens.css, status-classes.css, utils.js and the one-job modules split
// out of it are all reachable through Files. If embed.FS isn't picking up an asset, every consumer
// silently 404s — catching it here is much better than catching it on
// the first plant.
func TestSharedFilesEmbedded(t *testing.T) {
	required := []string{"tokens.css", "status-classes.css", "utils.js", "dom.js"}
	for _, name := range required {
		f, err := Files.Open(name)
		if err != nil {
			t.Errorf("shared.Files: missing %s: %v", name, err)
			continue
		}
		body, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			t.Errorf("read %s: %v", name, err)
			continue
		}
		if len(body) == 0 {
			t.Errorf("%s is empty", name)
		}
	}
}

// TestUtilsJSHasExports is a smoke check that utils.js and the one-job
// modules split out of it advertise the helpers the style guide promises,
// each in the file a page imports it from. Doesn't run the JS — just asserts
// the export lines exist. A missing export is a regression that consumers
// will hit at module-load time with a non-obvious error.
func TestUtilsJSHasExports(t *testing.T) {
	for file, exports := range map[string][]string{
		"dom.js": {
			"export function escapeHtml",
			"export function h",
			"export function el",
			"export function delegateActions",
			"export function debounce",
		},
		"utils.js": {
			"export const api",
			"export function timeAgo",
			"export function formatTime",
			"export function formatClock",
			"export function formatDuration",
			"export function convertTimestamps",
			"export function createSSE",
			"export function showModal",
			"export function hideModal",
			"export function confirm",
			"export function prompt",
			"export function toast",
			"export function installBackdropClose",
			"export function installHtmxTimestampConversion",
		},
	} {
		body, err := fs.ReadFile(Files, file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		src := string(body)
		for _, e := range exports {
			if !strings.Contains(src, e) {
				t.Errorf("%s missing %q", file, e)
			}
		}
	}
}

// WHERE THE NODE TESTS LIVE. A *.test.js that pins a shared asset sits in
// testdata/js/, never beside the asset: the //go:embed *.css *.js *.svg in
// static.go takes every root-level .js into Files, both binaries embed Files,
// both serve it under /static/shared/, and go:embed has no exclude pattern.
// The two pins below hold that line: one reads what Files carries, the other
// the directory a new test would be committed into.

// TestSharedFiles_HoldNoTestJS walks Files and fails on any *.test.js in it.
// Core and the Edge serve only what Files holds, so a file the walk does not
// find is a URL no page and no request can reach.
func TestSharedFiles_HoldNoTestJS(t *testing.T) {
	files := 0
	err := fs.WalkDir(Files, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		files++
		if strings.HasSuffix(p, ".test.js") {
			t.Errorf("shared.Files embeds %s, a node test, and both binaries serve it: move it to testdata/js/%s", p, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk shared.Files: %v", err)
	}
	if files == 0 {
		t.Fatal("shared.Files is empty, so the walk proved nothing")
	}
}

// TestSharedRoot_HoldsNoTestJS reads the module root on disk, the directory
// the embed globs, so a new test committed beside its asset fails here,
// naming where it goes. testdata/js/ must still hold the tests: empty, it
// means the wrappers lost their files, not that the rule held.
func TestSharedRoot_HoldsNoTestJS(t *testing.T) {
	stray, err := filepath.Glob("*.test.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range stray {
		t.Errorf("%s is where the embed globs: move it to testdata/js/%s", p, p)
	}
	tests, err := filepath.Glob(filepath.Join("testdata", "js", "*.test.js"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tests) == 0 {
		t.Error("testdata/js/ holds no *.test.js: the node tests are not where their wrappers run them")
	}
}
