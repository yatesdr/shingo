package main

// styleindex.go — the chapter list in docs/ui-style-guide.md: every markdown
// file in docs/ui-style-guide/, in file-name order, linked and titled by its
// first heading (a chapter titles itself with `##`).

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const styleindexDir = "docs/ui-style-guide"

func init() {
	register(Generator{Name: styleindexDir, Emit: func(root string, args []string) (string, error) {
		if len(args) != 0 {
			return "", fmt.Errorf("takes no arguments, got %q", args)
		}
		return styleindexList(root)
	}})
}

func styleindexList(root string) (string, error) {
	dir := filepath.Join(root, filepath.FromSlash(styleindexDir))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "", fmt.Errorf("%s holds no chapters", styleindexDir)
	}
	link := filepath.Base(styleindexDir)
	var b strings.Builder
	for _, n := range names {
		raw, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return "", err
		}
		title := ""
		for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
			if t := strings.TrimLeft(line, "#"); t != line && strings.HasPrefix(t, " ") {
				title = strings.TrimSpace(t)
				break
			}
		}
		if title == "" {
			return "", fmt.Errorf("%s/%s has no heading", styleindexDir, n)
		}
		fmt.Fprintf(&b, "1. [`%s`](%s/%s) — %s\n", n, link, n, title)
	}
	return b.String(), nil
}
