package shared

import (
	"io"
	"io/fs"
	"strings"
	"testing"
)

// TestSharedFilesEmbedded asserts the foundational embed contract:
// tokens.css, status-classes.css, and utils.js are all reachable
// through Files. If embed.FS isn't picking up an asset, every consumer
// silently 404s — catching it here is much better than catching it on
// the first plant.
func TestSharedFilesEmbedded(t *testing.T) {
	required := []string{"tokens.css", "status-classes.css", "utils.js"}
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

// TestUtilsJSHasExports is a smoke check that utils.js advertises the
// helpers the style guide promises. Doesn't run the JS — just asserts
// the export lines exist. A missing export is a regression that
// consumers will hit at module-load time with a non-obvious error.
func TestUtilsJSHasExports(t *testing.T) {
	body, err := fs.ReadFile(Files, "utils.js")
	if err != nil {
		t.Fatalf("read utils.js: %v", err)
	}
	src := string(body)
	exports := []string{
		"export function escapeHtml",
		"export function h",
		"export function el",
		"export const api",
		"export function timeAgo",
		"export function formatTime",
		"export function formatDuration",
		"export function convertTimestamps",
		"export function createSSE",
		"export function showModal",
		"export function hideModal",
		"export function confirm",
		"export function prompt",
		"export function toast",
		"export function delegateActions",
		"export function installBackdropClose",
		"export function installHtmxTimestampConversion",
		"export function debounce",
	}
	for _, e := range exports {
		if !strings.Contains(src, e) {
			t.Errorf("utils.js missing %q", e)
		}
	}
}
