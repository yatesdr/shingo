package main

import (
	"strings"
	"testing"
)

const jsexportsFixture = `// ─── Section ───
//
// sorted sorts. A second sentence.
export function sorted(xs) {}

// Escapes HTML, e.g. ampersands. Prefer h.
const TABLE = {};
export function escape(s) {}

export const api = {};

// Loads a thing.
export async function load() {}

// A helper, not an export.
function internal() {}

export class Store {}
`

func TestJSExports_ListsEveryExport(t *testing.T) {
	root := gosrcWriteTree(t, map[string]string{"shared/utils.js": jsexportsFixture})
	got, err := jsexportsTable(root, "shared/utils.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{
		"| `sorted` | function | sorted sorts. |",
		"| `escape` | function | Escapes HTML, e.g. ampersands. |",
		"| `api` | const | — |",
		"| `load` | function | Loads a thing. |",
		"| `Store` | class | — |",
	} {
		if !strings.Contains(got, w+"\n") {
			t.Errorf("no row %s in\n%s", w, got)
		}
	}
	if strings.Contains(got, "internal") || strings.Contains(got, "Section") {
		t.Errorf("table carries a non-export or a divider:\n%s", got)
	}
}

func TestJSExports_TheRealModule(t *testing.T) {
	for file, names := range map[string][]string{
		"shared/dom.js":      {"escapeHtml", "h", "el", "debounce"},
		"shared/clock.js":    {"serverNow", "formatTime", "formatDuration"},
		"shared/list.js":     {"installTableSort", "reconcileList", "createStore"},
		"shared/settings.js": {"settingsPage", "durationFromText", "collectList"},
		"shared/utils.js":    {"api", "createSSE"},
	} {
		got, err := jsexportsTable(repoRoot, file)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			if !strings.Contains(got, "| `"+name+"` |") {
				t.Errorf("%s export %s is not listed", file, name)
			}
		}
	}
}
