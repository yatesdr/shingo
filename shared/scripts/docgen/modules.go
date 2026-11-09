package main

// modules.go — the root README's module table: every module go.work uses,
// its Go module path, and its one-line description.
//
// The description lives in the module's own go.mod, as the `//` comment
// directly above the `module` line (Go ignores it; `go mod edit -fmt` keeps
// it). A module without one is an error: the table has no other place to get
// it from, and a blank cell would be the hand copy this replaces.

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

func init() {
	register(Generator{
		Name: "go.work",
		Emit: func(root string, args []string) (string, error) {
			if len(args) > 0 {
				return "", fmt.Errorf("takes no arguments, got %q", args)
			}
			return modsEmit(root)
		},
	})
}

func modsEmit(root string) (string, error) {
	dirs, err := modsUse(filepath.Join(root, "go.work"))
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("| Module | Go module | Description |\n|---|---|---|\n")
	for _, dir := range dirs {
		modPath, desc, err := modsGoMod(filepath.Join(root, filepath.FromSlash(dir), "go.mod"))
		if err != nil {
			return "", err
		}
		if desc == "" {
			return "", fmt.Errorf("%s/go.mod has no description: add a `// <one line>` comment directly above its module line", dir)
		}
		fmt.Fprintf(&b, "| [**%s**](%s/) | `%s` | %s |\n", dir, dir, modPath, strings.ReplaceAll(desc, "|", `\|`))
	}
	return b.String(), nil
}

// modsUse is go.work's use directives, in file order, as clean slash paths.
func modsUse(gowork string) ([]string, error) {
	raw, err := os.ReadFile(gowork)
	if err != nil {
		return nil, err
	}
	var out []string
	block := false
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if i := strings.Index(line, "//"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		switch {
		case line == "use (":
			block = true
		case block && line == ")":
			block = false
		case block && line != "":
			out = append(out, path.Clean(line))
		case strings.HasPrefix(line, "use "):
			out = append(out, path.Clean(strings.TrimSpace(strings.TrimPrefix(line, "use "))))
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no use directives", gowork)
	}
	return out, nil
}

// modsGoMod is a go.mod's module path and the comment lines directly above
// its module line, joined.
func modsGoMod(gomod string) (modPath, desc string, err error) {
	raw, err := os.ReadFile(gomod)
	if err != nil {
		return "", "", err
	}
	var comment []string
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "//"):
			comment = append(comment, strings.TrimSpace(strings.TrimPrefix(t, "//")))
		case strings.HasPrefix(t, "module "):
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(t, "module ")), `"`), strings.Join(comment, " "), nil
		default:
			comment = nil
		}
	}
	return "", "", fmt.Errorf("%s: no module line", gomod)
}
