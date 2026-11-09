package main

import (
	"strings"
	"testing"
)

const tokensFixture = `/* --ghost: #000; a declaration in a comment is not one */
:root {
  --bg: var(--elev-base); /* page */
  --gap: calc(var(--sp-1) * 2);
  --robot-1: #22c9ae;
}

[data-theme="dark"] {
  --bg: #0d1117;
  --dark-only: #123456;
}

@media (prefers-reduced-motion: reduce) {
  :root { --bg: red; }
}

.btn { color: var(--bg); }
`

func TestTokens_ThemesSideBySide(t *testing.T) {
	root := gosrcWriteTree(t, map[string]string{"shared/tokens.css": tokensFixture})
	got, err := tokensThemes(root, "shared/tokens.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{
		"| `--bg` | `var(--elev-base)` | `#0d1117` |",
		"| `--gap` | `calc(var(--sp-1) * 2)` | — |",
		"| `--robot-1` | `#22c9ae` | — |",
		"| `--dark-only` | — | `#123456` |",
	} {
		if !strings.Contains(got, w+"\n") {
			t.Errorf("no row %s in\n%s", w, got)
		}
	}
	for _, n := range []string{"--ghost", "red"} {
		if strings.Contains(got, n) {
			t.Errorf("table carries %s (a comment, or the reduced-motion block):\n%s", n, got)
		}
	}
}

func TestTokens_OneBlockAndRefusals(t *testing.T) {
	root := gosrcWriteTree(t, map[string]string{"op.css": ":root {\n  --os-r1: var(--robot-1);\n}\n", "bad.css": ":root { --a: 1;\n"})
	got, err := tokensBlock(root, "op.css")
	if err != nil || !strings.Contains(got, "| `--os-r1` | `var(--robot-1)` |\n") {
		t.Errorf("operator block: %q, %v", got, err)
	}
	if _, err := tokensBlock(root, "bad.css"); err == nil {
		t.Error("unbalanced braces parsed without error")
	}
	if _, err := tokensThemes(root, "op.css"); err == nil {
		t.Error("a sheet with no dark block rendered as themes")
	}
}

// TestTokens_TheRealSheet: the shared sheet's light and dark text values, which
// the guide's prose once quoted wrong, are what the table says.
func TestTokens_TheRealSheet(t *testing.T) {
	got, err := tokensThemes(repoRoot, "shared/tokens.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "| `--text-muted` | `#68717a` | `#8b949e` |\n") {
		t.Errorf("--text-muted row is not #68717a / #8b949e:\n%s", got)
	}
}
