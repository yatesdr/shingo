package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
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

func remove(t *testing.T, root, name string) {
	t.Helper()
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(name))); err != nil {
		t.Fatal(err)
	}
}

func testFile(pkg string, names ...string) string {
	var b strings.Builder
	b.WriteString("package " + pkg + "\n\nimport \"testing\"\n")
	for _, n := range names {
		b.WriteString("\nfunc " + n + "(t *testing.T) {}\n")
	}
	return b.String()
}

// fixture is a two-module repo committed once; it returns the root and the
// base commit.
func fixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	run(t, root, "init", "-q")
	run(t, root, "config", "user.email", "fixture@example.invalid")
	run(t, root, "config", "user.name", "fixture")
	run(t, root, "config", "core.autocrlf", "false")
	write(t, root, map[string]string{
		"protocol/go.mod":                          "module shingo/protocol\n\ngo 1.25.0\n",
		"protocol/wire_test.go":                    testFile("protocol", "TestWire"),
		"shingo-edge/go.mod":                       "module shingoedge\n\ngo 1.25.0\n",
		"shingo-edge/engine/a_test.go":             testFile("engine", "TestA", "TestB"),
		"shingo-edge/engine/b_test.go":             testFile("engine", "TestC"),
		"shingo-edge/engine/c_docker_test.go":      "//go:build docker\n\n" + testFile("engine", "TestD"),
		"shingo-edge/engine/x_test.go":             "package engine_test\n\nimport \"testing\"\n\nfunc TestExternal(t *testing.T) {}\n\nfunc (s suite) TestMethod(t *testing.T) {}\n\ntype suite struct{}\n\nfunc TestMain(m *testing.M) { m.Run() }\n\nfunc Testable() {}\n",
		"shingo-edge/www/w_test.go":                testFile("www", "TestW"),
		"shingo-edge/www/testdata/fixture_test.go": testFile("fixture", "TestNotATest"),
	})
	run(t, root, "add", "-A")
	run(t, root, "commit", "-q", "-m", "base")
	return root, run(t, root, "rev-parse", "HEAD")
}

func names(t *testing.T, root, base, ledger string) []string {
	t.Helper()
	gone, err := Check(root, base, []byte(ledger))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, v := range gone {
		out = append(out, v.Pkg+"."+v.Name)
	}
	return out
}

func TestCheck_TipIsTheBase(t *testing.T) {
	t.Parallel()
	root, base := fixture(t)
	if got := names(t, root, base, ""); got != nil {
		t.Fatalf("nothing changed, yet vanished = %v", got)
	}
}

func TestCheck_VanishedWithoutARowIsRed(t *testing.T) {
	t.Parallel()
	root, base := fixture(t)
	write(t, root, map[string]string{"shingo-edge/engine/a_test.go": testFile("engine", "TestA")})
	if got, want := names(t, root, base, ""), []string{"shingoedge/engine.TestB"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("vanished = %v, want %v", got, want)
	}
}

const rowB = "### DUP — `TestB`\n- test: `TestB` · pkg `shingoedge/engine` · run `-run '^TestB$'` · `shingo-edge/engine/a_test.go:7` @ `0000000` · tags `none`\n- code: DUP\n"

func TestCheck_VanishedWithARowIsGreen(t *testing.T) {
	t.Parallel()
	root, base := fixture(t)
	write(t, root, map[string]string{"shingo-edge/engine/a_test.go": testFile("engine", "TestA")})
	if got := names(t, root, base, rowB); got != nil {
		t.Fatalf("the row names it, yet vanished = %v", got)
	}
}

func TestCheck_ARowForAnotherPackageDoesNotCount(t *testing.T) {
	t.Parallel()
	root, base := fixture(t)
	write(t, root, map[string]string{"shingo-edge/engine/a_test.go": testFile("engine", "TestA")})
	ledger := strings.ReplaceAll(rowB, "pkg `shingoedge/engine`", "pkg `shingoedge/www`")
	if got, want := names(t, root, base, ledger), []string{"shingoedge/engine.TestB"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("vanished = %v, want %v", got, want)
	}
}

func TestCheck_RenamedIsRedWithoutARow(t *testing.T) {
	t.Parallel()
	root, base := fixture(t)
	write(t, root, map[string]string{"shingo-edge/engine/a_test.go": testFile("engine", "TestA", "TestB_Renamed")})
	if got, want := names(t, root, base, ""), []string{"shingoedge/engine.TestB"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("vanished = %v, want %v", got, want)
	}
}

func TestCheck_MovedWithinThePackageIsGreen(t *testing.T) {
	t.Parallel()
	root, base := fixture(t)
	// One fixture test to a tracked file, one to a new file git does not know yet.
	write(t, root, map[string]string{
		"shingo-edge/engine/a_test.go":   testFile("engine", "TestA", "TestC"),
		"shingo-edge/engine/new_test.go": testFile("engine", "TestB"),
	})
	remove(t, root, "shingo-edge/engine/b_test.go")
	if got := names(t, root, base, ""); got != nil {
		t.Fatalf("moved within the package, yet vanished = %v", got)
	}
}

func TestCheck_MovedToAnotherPackageIsRedWithoutARow(t *testing.T) {
	t.Parallel()
	root, base := fixture(t)
	write(t, root, map[string]string{
		"shingo-edge/www/w_test.go":    testFile("www"),
		"shingo-edge/engine/w_test.go": testFile("engine", "TestW"),
	})
	if got, want := names(t, root, base, ""), []string{"shingoedge/www.TestW"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("vanished = %v, want %v", got, want)
	}
}

func TestCheck_CommittedAndDeletedFilesCount(t *testing.T) {
	t.Parallel()
	root, base := fixture(t)
	remove(t, root, "shingo-edge/engine/c_docker_test.go")
	remove(t, root, "protocol/wire_test.go")
	run(t, root, "add", "-A")
	run(t, root, "commit", "-q", "-m", "tip")
	want := []string{"shingo/protocol.TestWire", "shingoedge/engine.TestD"}
	if got := names(t, root, base, ""); !reflect.DeepEqual(got, want) {
		t.Fatalf("vanished = %v, want %v", got, want)
	}
}

func TestCheck_OnlyTopLevelTestFunctionsAndRealTestFilesCount(t *testing.T) {
	t.Parallel()
	root, base := fixture(t)
	write(t, root, map[string]string{
		"shingo-edge/engine/x_test.go":             "package engine_test\n\nimport \"testing\"\n\nfunc TestExternal(t *testing.T) {}\n",
		"shingo-edge/www/testdata/fixture_test.go": "package fixture\n",
	})
	if got := names(t, root, base, ""); got != nil {
		t.Fatalf("a method, TestMain, a Testable helper and a testdata file are not tests, yet vanished = %v", got)
	}
}

func TestVanished_Skeleton(t *testing.T) {
	t.Parallel()
	root, base := fixture(t)
	remove(t, root, "shingo-edge/engine/c_docker_test.go")
	gone, err := Check(root, base, nil)
	if err != nil || len(gone) != 1 {
		t.Fatalf("Check = %v, %v; want TestD", gone, err)
	}
	got := gone[0].Skeleton(base)
	want := "### <CODE> — `TestD`\n" +
		"- test: `TestD` · pkg `shingoedge/engine` · run `-run '^TestD$'` · `shingo-edge/engine/c_docker_test.go:7` @ `" + base[:8] + "` · tags `docker`\n" +
		"- code: <CODE>\n- assertion map: \n- mutation: \n- coverage Δ: \n- citations rewritten: \n- verified at: `<sha>`\n"
	if got != want {
		t.Fatalf("Skeleton =\n%s\nwant\n%s", got, want)
	}
	// A filled skeleton is a row the check accepts.
	if !Rows([]byte(got))[[2]string{"shingoedge/engine", "TestD"}] {
		t.Fatal("the skeleton's own - test: line does not parse as a row")
	}
}

func TestRows_TheLedgerShape(t *testing.T) {
	t.Parallel()
	ledger := "# Test population ledger\n\n## Rows\n\n" +
		"### DUP — `TestName`\n" +
		"- test: `TestName` · pkg `shingoedge/engine` · run `-run '^TestName$'` · `shingo-edge/engine/x_test.go:55` @ `22c81558` · tags `none`\n" +
		"- code: DUP\n\n" +
		"### FOLD — `TestParent/case`\n" +
		"- test: `TestParent/case` · pkg `shingocore/store` · run `-run '^TestParent$/^case$'` · tags `docker`\n" +
		"- mutation: names `TestOther` · pkg `not/a/row`\n"
	want := map[[2]string]bool{
		{"shingoedge/engine", "TestName"}:       true,
		{"shingocore/store", "TestParent/case"}: true,
	}
	if got := Rows([]byte(ledger)); !reflect.DeepEqual(got, want) {
		t.Fatalf("Rows = %v, want %v", got, want)
	}
}

func TestResolveBase_DefaultsToTheMergeBaseWithOriginMain(t *testing.T) {
	t.Parallel()
	root, base := fixture(t)
	if _, err := ResolveBase(root, ""); err == nil {
		t.Fatal("no origin/main, yet a base resolved; it must say so instead of checking nothing")
	}
	run(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")
	write(t, root, map[string]string{"shingo-edge/engine/a_test.go": testFile("engine", "TestA")})
	run(t, root, "commit", "-q", "-am", "on the branch")
	got, err := ResolveBase(root, "")
	if err != nil || got != base {
		t.Fatalf("ResolveBase = %q, %v; want the merge base %s", got, err, base)
	}
	if got, err := ResolveBase(root, "HEAD"); err != nil || got == base {
		t.Fatalf("ResolveBase(HEAD) = %q, %v; want the branch tip, not the base", got, err)
	}
}
