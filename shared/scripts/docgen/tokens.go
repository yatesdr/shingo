package main

// tokens.go — the token tables in docs/ui-style-guide/04-design-tokens.md:
//
//   - `shared/tokens.css`: every custom property the shared `:root` block
//     declares, with its light value and the value the `[data-theme="dark"]`
//     block overrides it with (a token only the dark block declares is listed
//     after the rest);
//   - `shingo-edge/www/static/operator-station/operator.css`: the operator
//     station's own `:root` block, aliases and all.
//
// Only top-level blocks are read: the reduced-motion `@media` block's `:root`
// is not a theme. Source order, which is the file's own grouping.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

func init() {
	register(Generator{Name: "shared/tokens.css", Emit: func(root string, args []string) (string, error) {
		if len(args) != 0 {
			return "", fmt.Errorf("takes no arguments, got %q", args)
		}
		return tokensThemes(root, "shared/tokens.css")
	}})
	register(Generator{Name: "shingo-edge/www/static/operator-station/operator.css", Emit: func(root string, args []string) (string, error) {
		if len(args) != 0 {
			return "", fmt.Errorf("takes no arguments, got %q", args)
		}
		return tokensBlock(root, "shingo-edge/www/static/operator-station/operator.css")
	}})
}

// tokensDecl is one custom property declaration.
type tokensDecl struct{ name, value string }

var tokensComment = regexp.MustCompile(`(?s)/\*.*?\*/`)

// tokensBlocks is a stylesheet's top-level rule blocks: selector → the custom
// properties it declares, in order. A selector that appears twice at top level
// keeps its first block.
func tokensBlocks(css string) (map[string][]tokensDecl, error) {
	css = tokensComment.ReplaceAllString(css, " ")
	out := map[string][]tokensDecl{}
	depth, start, bodyStart := 0, 0, 0
	selector := ""
	for i, c := range css {
		switch c {
		case '{':
			if depth == 0 {
				selector = strings.Join(strings.Fields(css[start:i]), " ")
				bodyStart = i + 1
			}
			depth++
		case '}':
			depth--
			if depth < 0 {
				return nil, fmt.Errorf("unbalanced braces")
			}
			if depth == 0 {
				if _, dup := out[selector]; !dup {
					out[selector] = tokensDecls(css[bodyStart:i])
				}
				start = i + 1
			}
		}
	}
	if depth != 0 {
		return nil, fmt.Errorf("unbalanced braces")
	}
	return out, nil
}

// tokensDecls is a block body's custom property declarations. A nested block
// (only in an @media body) is not split into, which is why it is not read.
func tokensDecls(body string) []tokensDecl {
	var out []tokensDecl
	for _, d := range strings.Split(body, ";") {
		d = strings.TrimSpace(d)
		if !strings.HasPrefix(d, "--") {
			continue
		}
		name, value, ok := strings.Cut(d, ":")
		if !ok {
			continue
		}
		out = append(out, tokensDecl{strings.TrimSpace(name), strings.Join(strings.Fields(value), " ")})
	}
	return out
}

func tokensRead(root, file string) (map[string][]tokensDecl, error) {
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file)))
	if err != nil {
		return nil, err
	}
	blocks, err := tokensBlocks(string(raw))
	if err != nil {
		return nil, fmt.Errorf("%s: %v", file, err)
	}
	return blocks, nil
}

// tokensThemes renders the light and dark blocks side by side.
func tokensThemes(root, file string) (string, error) {
	blocks, err := tokensRead(root, file)
	if err != nil {
		return "", err
	}
	light, dark := blocks[":root"], blocks[`[data-theme="dark"]`]
	if len(light) == 0 || len(dark) == 0 {
		return "", fmt.Errorf(`%s: want a top-level :root and [data-theme="dark"] block, each declaring tokens`, file)
	}
	darkOf := map[string]string{}
	for _, d := range dark {
		darkOf[d.name] = d.value
	}
	var b strings.Builder
	b.WriteString("| Token | Light (`:root`) | Dark (`[data-theme=\"dark\"]`) |\n|---|---|---|\n")
	seen := map[string]bool{}
	for _, d := range light {
		seen[d.name] = true
		dv := "—"
		if v, ok := darkOf[d.name]; ok {
			dv = "`" + v + "`"
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s |\n", d.name, gosrcCell("`"+d.value+"`"), gosrcCell(dv))
	}
	for _, d := range dark {
		if !seen[d.name] {
			fmt.Fprintf(&b, "| `%s` | — | %s |\n", d.name, gosrcCell("`"+d.value+"`"))
		}
	}
	return b.String(), nil
}

// tokensBlock renders one stylesheet's :root block.
func tokensBlock(root, file string) (string, error) {
	blocks, err := tokensRead(root, file)
	if err != nil {
		return "", err
	}
	decls := blocks[":root"]
	if len(decls) == 0 {
		return "", fmt.Errorf("%s: no top-level :root block declaring tokens", file)
	}
	var b strings.Builder
	b.WriteString("| Token | Value |\n|---|---|\n")
	for _, d := range decls {
		fmt.Fprintf(&b, "| `%s` | %s |\n", d.name, gosrcCell("`"+d.value+"`"))
	}
	return b.String(), nil
}
