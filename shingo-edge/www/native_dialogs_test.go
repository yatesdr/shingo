package www

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// TestPageDialogsAreTheLayers: a page script that calls confirm, prompt or
// alert binds that name with an import from shared/layer.js. A bare call that
// nothing imports is the browser's own dialog (window.confirm): unstyled,
// outside the layer's one overlay stack, and blocking the page's thread. The
// Production page's "Clear this lineside bucket?" was one (production.js
// clearLinesideBucket, census flat bug, 2026-11-11). shared/layer.js has no
// alert, so an alert is always the browser's.
func TestPageDialogsAreTheLayers(t *testing.T) {
	t.Parallel()
	call := regexp.MustCompile(`(^|[^.\w$])(confirm|prompt|alert)\s*\(`)
	layerImport := regexp.MustCompile(`(?s)import\s*\{([^}]*)\}\s*from\s*['"]/static/shared/layer\.js['"]`)
	err := fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".js") || strings.HasSuffix(p, ".min.js") || strings.Contains(p, "/vendor/") {
			return nil
		}
		body, err := fs.ReadFile(staticFS, p)
		if err != nil {
			t.Errorf("read %s: %v", p, err)
			return nil
		}
		bound := map[string]bool{}
		for _, m := range layerImport.FindAllStringSubmatch(string(body), -1) {
			for _, name := range strings.Split(m[1], ",") {
				f := strings.Fields(name) // "confirm" or "confirm as uiConfirm"
				if len(f) == 1 {
					bound[f[0]] = true
				}
			}
		}
		for i, line := range strings.Split(string(body), "\n") {
			code := strings.TrimSpace(line)
			if strings.HasPrefix(code, "//") || strings.HasPrefix(code, "*") {
				continue
			}
			for _, m := range call.FindAllStringSubmatch(code, -1) {
				if name := m[2]; !bound[name] && !strings.Contains(code, "function "+name) {
					t.Errorf("%s:%d calls %s() without importing it from /static/shared/layer.js: the browser's own dialog\n  %s",
						p, i+1, name, code)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk static: %v", err)
	}
}
