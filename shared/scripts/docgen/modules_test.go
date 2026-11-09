package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The table follows go.work's order, takes each description from the comment
// directly above the module line (a comment separated from it by another line
// does not count), and refuses a module with none.
func TestMods_TableFromGoWork(t *testing.T) {
	root := t.TempDir()
	write := func(name, body string) {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.work", "go 1.25.0\n\nuse (\n\t./zeta // the last\n\t./alpha\n)\n\nuse ./solo\n")
	write("zeta/go.mod", "// Zeta does | things.\nmodule example/zeta\n\ngo 1.25.0\n")
	write("alpha/go.mod", "// An old note.\n\n// Alpha, the first\n// on two lines.\nmodule example/alpha\n")
	write("solo/go.mod", "// Solo.\nmodule \"example/solo\"\n")
	got, err := modsEmit(root)
	if err != nil {
		t.Fatal(err)
	}
	want := "| Module | Go module | Description |\n|---|---|---|\n" +
		"| [**zeta**](zeta/) | `example/zeta` | Zeta does \\| things. |\n" +
		"| [**alpha**](alpha/) | `example/alpha` | Alpha, the first on two lines. |\n" +
		"| [**solo**](solo/) | `example/solo` | Solo. |\n"
	if got != want {
		t.Errorf("table:\n%s\nwant:\n%s", got, want)
	}

	write("solo/go.mod", "module example/solo\n")
	if _, err := modsEmit(root); err == nil || !strings.Contains(err.Error(), "solo/go.mod has no description") {
		t.Errorf("a module with no description was not refused: %v", err)
	}
}

// On the real tree: all five workspace modules, each described.
func TestMods_RealTree(t *testing.T) {
	got, err := modsEmit(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"`shingo/integration`", "`shingo/protocol`", "`shingo/shared`", "`shingocore`", "`shingoedge`"} {
		if !strings.Contains(got, m) {
			t.Errorf("module %s is not in the table:\n%s", m, got)
		}
	}
}
