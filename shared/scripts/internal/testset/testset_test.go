package testset

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func tests(pkg string, names ...string) string {
	var b strings.Builder
	b.WriteString("package " + pkg + "\n\nimport \"testing\"\n")
	for _, n := range names {
		b.WriteString("\nfunc " + n + "(t *testing.T) {}\n")
	}
	return b.String()
}

func TestAdded_WhatTheTipDeclaresAndTheBaseDoesNot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	git(t, root, "init", "-q")
	git(t, root, "config", "user.email", "fixture@example.invalid")
	git(t, root, "config", "user.name", "fixture")
	git(t, root, "config", "core.autocrlf", "false")
	write(t, root, map[string]string{
		"shingo-core/go.mod":           "module shingocore\n",
		"shingo-core/eng/a_test.go":    tests("eng", "TestKept", "TestMoved", "TestRenamed"),
		"shingo-core/other/b_test.go":  tests("other", "TestUntouched"),
		"shingo-edge/go.mod":           "module shingoedge\n",
		"shingo-edge/store/c_test.go":  tests("store", "TestAlready"),
		"shingo-edge/store/testdata/x": "not a test",
	})
	git(t, root, "add", "-A")
	git(t, root, "commit", "-q", "-m", "base")
	base := git(t, root, "rev-parse", "HEAD")

	write(t, root, map[string]string{
		// The moved test goes to another file of its package: not new. The renamed
		// one is new under its new name, and so is the one this commit adds.
		"shingo-core/eng/a_test.go": tests("eng", "TestKept", "TestRenamedNow", "TestCommitted"),
		"shingo-core/eng/d_test.go": tests("eng", "TestMoved"),
	})
	git(t, root, "add", "-A")
	git(t, root, "commit", "-q", "-m", "tip")
	write(t, root, map[string]string{
		// Uncommitted, and untracked in a new package: both new.
		"shingo-edge/store/c_test.go":          tests("store", "TestAlready", "TestUncommitted"),
		"shingo-edge/fresh/e_test.go":          tests("fresh", "TestUntracked", "TestMain", "Testable"),
		"shingo-edge/fresh/testdata/f_test.go": tests("fresh", "TestInTestdata"),
	})
	got, err := Added(root, base)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, a := range got {
		names = append(names, a.Dir+" "+a.Name+" "+a.File)
	}
	want := []string{
		"shingo-core/eng TestCommitted shingo-core/eng/a_test.go",
		"shingo-core/eng TestRenamedNow shingo-core/eng/a_test.go",
		"shingo-edge/fresh TestUntracked shingo-edge/fresh/e_test.go",
		"shingo-edge/store TestUncommitted shingo-edge/store/c_test.go",
	}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("Added =\n%s\nwant\n%s", strings.Join(names, "\n"), strings.Join(want, "\n"))
	}
	if got[0].Line != 9 {
		t.Errorf("TestCommitted at line %d, want 9", got[0].Line)
	}
	if got, err := Added(root, "HEAD"); err != nil || len(got) != 2 {
		t.Errorf("Added against the tip = %v, %v; want the two uncommitted tests", got, err)
	}
}
