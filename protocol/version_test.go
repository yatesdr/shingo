package protocol_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"
)

// TestModuleVersionConsistency asserts that all three modules in the monorepo
// use the same Go version and the same golang.org/x/crypto version.
// Run: cd protocol && go test -run TestModuleVersionConsistency
func TestModuleVersionConsistency(t *testing.T) {
	type modInfo struct {
		name   string
		goVer  string
		crypto string
	}

	paths := []struct {
		name string
		path string
	}{
		{"protocol", "go.mod"},
		{"shingo-core", filepath.Join("..", "shingo-core", "go.mod")},
		{"shingo-edge", filepath.Join("..", "shingo-edge", "go.mod")},
	}

	var mods []modInfo
	for _, p := range paths {
		data, err := os.ReadFile(p.path)
		if err != nil {
			t.Fatalf("read %s go.mod: %v", p.name, err)
		}
		f, err := modfile.Parse(p.path, data, nil)
		if err != nil {
			t.Fatalf("parse %s go.mod: %v", p.name, err)
		}
		info := modInfo{name: p.name, goVer: f.Go.Version}
		for _, req := range f.Require {
			if req.Mod.Path == "golang.org/x/crypto" {
				info.crypto = req.Mod.Version
			}
		}
		mods = append(mods, info)
	}

	t.Run("GoVersionAlignment", func(t *testing.T) {
		base := mods[0].goVer
		drift := false
		for _, m := range mods[1:] {
			if m.goVer != base {
				drift = true
			}
		}
		if drift {
			var sb strings.Builder
			for _, m := range mods {
				fmt.Fprintf(&sb, "  %-12s go %s\n", m.name, m.goVer)
			}
			t.Errorf("go version drift detected:\n%s\nAll modules should use go %s (protocol is the reference)", sb.String(), base)
		}
	})

	t.Run("XCryptoAlignment", func(t *testing.T) {
		var versions []string
		for _, m := range mods {
			if m.crypto != "" {
				versions = append(versions, m.crypto)
			}
		}
		if len(versions) < 2 {
			t.Skip("fewer than 2 modules depend on x/crypto")
		}
		base := versions[0]
		drift := false
		for _, v := range versions[1:] {
			if v != base {
				drift = true
			}
		}
		if drift {
			var sb strings.Builder
			for _, m := range mods {
				ver := m.crypto
				if ver == "" {
					ver = "(indirect/none)"
				}
				fmt.Fprintf(&sb, "  %-12s %s\n", m.name, ver)
			}
			t.Errorf("x/crypto version drift:\n%s\nRun 'go get golang.org/x/crypto@%s && go mod tidy' in drifted modules", sb.String(), base)
		}
	})
}
