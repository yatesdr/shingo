package main

// jsexports.go — the export lists in
// docs/ui-style-guide/17-javascript-primitives.md, one table per shared module
// in jsexportsFiles: every top-level `export` in the file, its kind, and the
// first sentence of the `//` comment block written directly above it (one
// declaration line between the two is allowed: a helper constant the function
// reads, like escapeHtml's table).

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// jsexportsFiles are the shared modules utils.js was split into (one job
// each), and what is left of it; each feeds the block named after it.
var jsexportsFiles = []string{"shared/dom.js", "shared/utils.js"}

func init() {
	for _, file := range jsexportsFiles {
		register(Generator{Name: file, Emit: func(root string, args []string) (string, error) {
			if len(args) != 0 {
				return "", fmt.Errorf("takes no arguments, got %q", args)
			}
			return jsexportsTable(root, file)
		}})
	}
}

var (
	jsexportsLine = regexp.MustCompile(`^export\s+(?:async\s+)?(function\*?|const|let|var|class)\s+([A-Za-z_$][\w$]*)`)
	jsexportsDecl = regexp.MustCompile(`^(const|let|var)\s`)
)

// jsexportsTable renders the exports in file order.
func jsexportsTable(root, file string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file)))
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	var b strings.Builder
	b.WriteString("| Export | Kind | What it does |\n|---|---|---|\n")
	n := 0
	for i, line := range lines {
		m := jsexportsLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		n++
		kind := strings.TrimSuffix(m[1], "*")
		doc := gosrcFirstSentence(jsexportsComment(lines, i))
		if doc == "" {
			doc = "—"
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s |\n", m[2], kind, gosrcCell(doc))
	}
	if n == 0 {
		return "", fmt.Errorf("%s exports nothing", file)
	}
	return b.String(), nil
}

// jsexportsComment is the text of the // block directly above line i, or
// above the one declaration line directly above it.
func jsexportsComment(lines []string, i int) string {
	j := i - 1
	if j >= 0 && jsexportsDecl.MatchString(strings.TrimSpace(lines[j])) {
		j--
	}
	var block []string
	for ; j >= 0; j-- {
		t := strings.TrimSpace(lines[j])
		if !strings.HasPrefix(t, "//") {
			break
		}
		text := strings.TrimSpace(strings.TrimPrefix(t, "//"))
		if strings.Contains(text, "───") {
			break // a section divider: what is above it is not this export's
		}
		block = append([]string{text}, block...)
	}
	return strings.Join(block, "\n")
}
