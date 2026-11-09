package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"shingo/shared/scripts/internal/docindex"
)

// repo is a temp git repo the tests commit to.
type repo struct {
	t   *testing.T
	dir string
}

func newRepo(t *testing.T) *repo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	r := &repo{t, t.TempDir()}
	r.git("init", "-q")
	return r
}

func (r *repo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.dir, "-c", "user.name=t", "-c", "user.email=t@example.com",
		"-c", "core.autocrlf=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *repo) write(files map[string]string) {
	r.t.Helper()
	for p, body := range files {
		full := filepath.Join(r.dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			r.t.Fatal(err)
		}
	}
}

func (r *repo) commit(msg string, files map[string]string) string {
	r.t.Helper()
	r.write(files)
	r.git("add", "-A")
	r.git("commit", "-q", "-m", msg)
	return r.git("rev-parse", "--short=8", "HEAD")
}

func (r *repo) read(p string) string {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.dir, filepath.FromSlash(p)))
	if err != nil {
		r.t.Fatal(err)
	}
	return string(b)
}

func (r *repo) index() *docindex.Index {
	r.t.Helper()
	ix, err := docindex.Load(r.dir)
	if err != nil {
		r.t.Fatal(err)
	}
	return ix
}

const laneGo = "package engine\n\nfunc TryLockFor() {}\n\nfunc helperName() {}\n"

// history builds: base (the docs and the lane), a side commit off it that
// HEAD never sees, and HEAD, which renames TryLockFor. It returns their
// short shas.
func history(t *testing.T) (r *repo, base, side, head string) {
	r = newRepo(t)
	base = r.commit("base", map[string]string{
		"shingo-core/engine/lane.go": laneGo,
		"docs/a.md":                  "# A\n\n`TryLockFor` holds the lane.\n",
		"docs/b.md":                  "# B\n\nSee `engine/lane.go`.\n",
		"docs/c.md":                  "# C\n\nNothing the change names: `helperName`.\n",
		"docs/d.md":                  "No heading at all.\n",
		"README.md":                  "# R\n\n[A](docs/a.md)\n",
		"CHANGELOG.md":               "# Changelog\n",
	})
	r.git("checkout", "-q", "-b", "side")
	side = r.commit("side", map[string]string{"side.txt": "x\n"})
	r.git("checkout", "-q", "-")
	head = r.commit("rename", map[string]string{
		"shingo-core/engine/lane.go": strings.Replace(laneGo, "TryLockFor()", "TryLockForOrder()", 1),
	})
	return r, base, side, head
}

func stamp(title, sha, body string) string {
	return title + "\n\nVerified against: " + sha + "\n\n" + body
}

func TestCheck(t *testing.T) {
	r, base, side, _ := history(t)
	r.write(map[string]string{
		"docs/a.md": stamp("# A", base, "`TryLockFor` holds the lane.\n"),
		"docs/b.md": stamp("# B", side, "See `engine/lane.go`.\n"),
		"docs/c.md": stamp("# C", "deadbeef", "Nothing.\n"),
	})
	var got []string
	for _, f := range Check(r.dir, r.index()) {
		got = append(got, f.String())
	}
	want := []string{
		"README.md:1: no \"Verified against: <sha>\" line under the title",
		"docs/b.md:3: Verified against: " + side + " is not an ancestor of HEAD",
		"docs/c.md:3: Verified against: deadbeef names no commit in this repo",
		"docs/d.md: no title to stamp under: the doc has no # heading",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Check =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestWrite(t *testing.T) {
	r, _, _, head := history(t)
	r.write(map[string]string{"docs/b.md": stamp("# B", head, "See `engine/lane.go`.\n")})
	failing := filepath.Join(t.TempDir(), "failing")
	if err := os.WriteFile(failing, []byte("docs/c.md\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	list, err := readList(failing)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Write(r.dir, r.index(), list, head, &out); err != nil {
		t.Fatal(err)
	}
	wantOut := "stamped 2 doc(s) Verified against: " + head + " (1 already were); skipped 2\n" +
		"  skipped docs/c.md: a reader found fault with it\n" +
		"  skipped docs/d.md: no title to stamp under\n"
	if out.String() != wantOut {
		t.Errorf("Write printed\n%s\nwant\n%s", out.String(), wantOut)
	}
	if got, want := r.read("docs/a.md"), stamp("# A", head, "`TryLockFor` holds the lane.\n"); got != want {
		t.Errorf("docs/a.md = %q, want %q: inserted", got, want)
	}
	if got := r.read("docs/c.md"); strings.Contains(got, "Verified against") {
		t.Errorf("docs/c.md was stamped though it is listed as failing: %q", got)
	}
	if got := r.read("CHANGELOG.md"); got != "# Changelog\n" {
		t.Errorf("CHANGELOG.md, which is exempt, was written: %q", got)
	}
	if fs := Check(r.dir, r.index()); len(fs) != 2 {
		t.Errorf("after the write, Check = %v; want only the failing doc and the untitled one", fs)
	}
	if l, err := readList("/dev/null"); err != nil || len(l) != 0 {
		t.Errorf("readList(/dev/null) = %v, %v; want the empty list", l, err)
	}
}

func TestTouched(t *testing.T) {
	r, base, _, _ := history(t)
	got, err := Touched(r.dir, r.index(), base)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"docs/a.md:3: TryLockFor (declared on a changed line of shingo-core/engine/lane.go)",
		"docs/b.md:3: engine/lane.go (shingo-core/engine/lane.go changed)",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Touched =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestChangedLines(t *testing.T) {
	diff := "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -3 +3,2 @@\n-a\n+b\n+c\n@@ -10,0 +12 @@\n+d\n" +
		"diff --git a/new.go b/new.go\n--- /dev/null\n+++ b/new.go\n@@ -0,0 +1,2 @@\n+x\n+y\n"
	removed, added := changedLines([]byte(diff))
	if want := map[string]map[int]bool{"x.go": {3: true}}; !reflect.DeepEqual(removed, want) {
		t.Errorf("removed = %v, want %v", removed, want)
	}
	if want := map[string]map[int]bool{"x.go": {3: true, 4: true, 12: true}, "new.go": {1: true, 2: true}}; !reflect.DeepEqual(added, want) {
		t.Errorf("added = %v, want %v", added, want)
	}
}

func TestDeclaredOn(t *testing.T) {
	src := "package x\n\nfunc A() {}\n\nfunc (T) Method() {}\n\ntype T struct{ field int }\n\nconst (\n\tK = 1\n)\n\nvar V, W = 1, 2\n"
	got := declaredOn([]byte(src), map[int]bool{3: true, 5: true, 7: true, 10: true, 13: true})
	if want := []string{"A", "Method", "T", "K", "V", "W"}; !reflect.DeepEqual(got, want) {
		t.Errorf("declaredOn = %q, want %q", got, want)
	}
}
