package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"a/loader_windows.go":       "package a\n",
		"a/staged_arm_test.go":      "package a\n",
		"a/ok_windows_test.go":      "//go:build windows\n\npackage a\n",
		"a/x_linux_amd64.go":        "//go:build linux && amd64\r\n\r\npackage a\r\n",
		"a/testdata/z_linux.go":     "package a\n",
		"b/node_modules/q_linux.go": "package a\n",
		"c/plain.go":                "package a\n",
		"c/_test.go":                "package a\n",
		"c/y_arm.go":                "//go:build armbe\npackage a\n", // armbe is not the word arm
		"c/old_arm64.go":            "// +build arm64\npackage a\n",  // the old form is not read
		"c/late_darwin.go":          "package a\n\n" + strings.Repeat("/", 2100) + "\n//go:build darwin\n",
	}
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var sb strings.Builder
	if check(root, &sb) {
		t.Fatal("check passed a tree with name-excluded files")
	}
	want := `FAIL a/loader_windows.go
     ends in _windows, which Go reads as a GOOS/GOARCH build constraint, so this file is
     NOT COMPILED on other platforms: what it declares is missing there.
     Rename it (loader_windows.go -> loader_windows_choice.go, say), or add a //go:build line naming
     windows if the constraint is intended.
FAIL a/staged_arm_test.go
     ends in _arm, which Go reads as a GOOS/GOARCH build constraint, so this file is
     NOT COMPILED on other platforms and its tests silently do not exist there.
     Rename it (staged_arm_test.go -> staged_arm_gate_test.go), or add a //go:build line naming
     arm if the constraint is intended.
FAIL c/late_darwin.go
     ends in _darwin, which Go reads as a GOOS/GOARCH build constraint, so this file is
     NOT COMPILED on other platforms: what it declares is missing there.
     Rename it (late_darwin.go -> late_darwin_choice.go, say), or add a //go:build line naming
     darwin if the constraint is intended.
FAIL c/old_arm64.go
     ends in _arm64, which Go reads as a GOOS/GOARCH build constraint, so this file is
     NOT COMPILED on other platforms: what it declares is missing there.
     Rename it (old_arm64.go -> old_arm64_choice.go, say), or add a //go:build line naming
     arm64 if the constraint is intended.
FAIL c/y_arm.go
     ends in _arm, which Go reads as a GOOS/GOARCH build constraint, so this file is
     NOT COMPILED on other platforms: what it declares is missing there.
     Rename it (y_arm.go -> y_arm_choice.go, say), or add a //go:build line naming
     arm if the constraint is intended.
`
	if sb.String() != want {
		t.Errorf("output:\n%s\nwant:\n%s", sb.String(), want)
	}
}

func TestCheck_CleanTree(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "plain.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	if !check(root, &sb) || sb.String() != "ok   Go filenames (no accidental GOOS/GOARCH build constraints)\n" {
		t.Errorf("clean tree: %q", sb.String())
	}
}
