package docindex

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestInScope(t *testing.T) {
	cases := map[string]bool{
		"docs/lanes.md":                             true,
		"docs/dev-env/kanban-rig.md":                true, // any depth under docs/
		"docs/ui-style-guide/04-tokens.md":          true,
		"README.md":                                 true,
		"README.dev.md":                             true,
		"plants/README.md":                          true,
		"shingo-core/README.md":                     true,
		"shingo-core/deploy/README.md":              true,
		"shingo-core/docs/configuration.md":         true,
		"shingo-edge/docs/backup-restore.md":        true,
		"protocol/migrate/README.md":                true,
		"shingo-core/docs/sub/deeper.md":            false, // module docs are one level
		"CHANGELOG.md":                              false, // exempt
		"docs/testing/population-ledger.md":         false, // exempt
		"docs/lanes.txt":                            false,
		"shared/README.md":                          false,
		"shingo-edge/www/static/vendor/x/README.md": false,
	}
	for p, want := range cases {
		if got := InScope(p); got != want {
			t.Errorf("InScope(%q) = %v, want %v", p, got, want)
		}
	}
}

// A doc that holds every shape the reader has to get right.
const shapesDoc = "Some prose before the title is not a title.\n" + // 1
	"\n" + // 2
	"## Chapter title\n" + // 3
	"\n" + // 4
	"A span `that wraps\n" + // 5
	"across lines` and ``a `tick` inside``, an escaped \\`tick\\`.\n" + // 6
	"See [the doc](../other.md#part \"title\") and ![a pic](img/a.png).\n" + // 7
	"<img src=\"pic.svg\" alt=\"x\"> and <source srcset=\"a.svg 1x, b.svg 2x\">\n" + // 8
	"An autolink <https://example.com/TestNotACitation> and a <!-- comment `x` --> here.\n" + // 9
	"| `one` | two ` |\n" + // 10
	"| three ` | `four` |\n" + // 11
	"<!-- generated: protocol/types.go by docgen -->\n" + // 12
	"<!--\n" + // 13
	"# not a heading\n" + // 14
	"-->\n" + // 15
	"```bash title\n" + // 16
	"# not a heading either\n" + // 17
	"go test -run TestX ./...\n" + // 18
	"```\n" + // 19
	"[ref]: ../ref.md\n" + // 20
	"- a list item `with code`\n" + // 21
	"- the next item\n" // 22

func TestParse_Shapes(t *testing.T) {
	d := Parse("docs/x/doc.md", []byte(shapesDoc))
	if d.Title != 3 {
		t.Errorf("Title = %d, want 3: the first ATX heading, any level, outside fences and comments", d.Title)
	}
	var codes []string
	for _, b := range d.Prose {
		for _, c := range b.Code {
			codes = append(codes, c.Text+"@"+itoa(c.Line))
		}
	}
	wantCodes := []string{"that wraps across lines@5", "a `tick` inside@6", "one@10", "four@11", "with code@21"}
	if !reflect.DeepEqual(codes, wantCodes) {
		t.Errorf("code spans = %q, want %q", codes, wantCodes)
	}
	var links []string
	for _, l := range d.Links {
		links = append(links, l.Target+"@"+itoa(l.Line))
	}
	wantLinks := []string{"../other.md#part@7", "img/a.png@7", "pic.svg@8", "a.svg@8", "b.svg@8", "../ref.md@20"}
	if !reflect.DeepEqual(links, wantLinks) {
		t.Errorf("links = %q, want %q", links, wantLinks)
	}
	if len(d.Sources) != 1 || d.Sources[0].Target != "protocol/types.go" || d.Sources[0].Line != 12 {
		t.Errorf("generated sources = %+v, want protocol/types.go at line 12", d.Sources)
	}
	if len(d.Fences) != 1 || d.Fences[0].Info != "bash title" || d.Fences[0].Line != 16 ||
		d.Fences[0].Body.Text != "# not a heading either\ngo test -run TestX ./..." || d.Fences[0].Body.Lines[1] != 18 {
		t.Errorf("fences = %+v", d.Fences)
	}
	var prose []string
	for _, b := range d.Prose {
		prose = append(prose, b.Text)
	}
	all := strings.Join(prose, "\n")
	for _, gone := range []string{"other.md", "img/a.png", "pic.svg", "example.com", "comment", "not a heading"} {
		if strings.Contains(all, gone) {
			t.Errorf("prose still holds %q, which is a link target, a tag, an autolink or a comment:\n%s", gone, all)
		}
	}
	for _, kept := range []string{"the doc", "a pic", "`that wraps"} {
		if !strings.Contains(all, kept) {
			t.Errorf("prose lost %q", kept)
		}
	}
}

func TestBlock_BareAndLineAt(t *testing.T) {
	d := Parse("a.md", []byte("# T\n\nx `code` y\nz `more\nwrap` w\n"))
	b := d.Prose[1]
	if got, want := b.Bare(), "x        y\nz      \n      w"; got != want {
		t.Errorf("Bare() = %q, want %q: the spans blanked, the line breaks kept", got, want)
	}
	if got := b.LineAt(strings.Index(b.Text, "z")); got != 4 {
		t.Errorf("LineAt = %d, want 4", got)
	}
	if got := b.Code[1].Line; got != 4 {
		t.Errorf("a wrapped span's Line = %d, want 4, the line it starts on", got)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestStamp(t *testing.T) {
	cases := []struct {
		name, doc, sha string
		line           int
		ok             bool
	}{
		{"present", "# T\n\nVerified against: abcdef12\n\nBody\n", "abcdef12", 3, true},
		{"chapter title", "## Chapter\nVerified against: 0123abcd\n", "0123abcd", 2, true},
		{"missing", "# T\n\nBody\n", "", 0, false},
		{"not first after the title", "# T\n\nBody\n\nVerified against: abcdef12\n", "", 0, false},
		{"not exact", "# T\n\nVerified against: abcdef12 (ok)\n", "", 0, false},
		{"upper-case hex is not a sha", "# T\n\nVerified against: ABCDEF12\n", "", 0, false},
		{"no title", "Body\n\nVerified against: abcdef12\n", "", 0, false},
	}
	for _, c := range cases {
		sha, line, ok := Parse("a.md", []byte(c.doc)).Stamp()
		if sha != c.sha || line != c.line || ok != c.ok {
			t.Errorf("%s: Stamp() = %q, %d, %v; want %q, %d, %v", c.name, sha, line, ok, c.sha, c.line, c.ok)
		}
	}
}

func TestSetStamp(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"inserted under the title", "# T\n\nBody\n", "# T\n\nVerified against: abcd1234\n\nBody\n"},
		{"a blank line kept before a glued paragraph", "# T\nBody\n", "# T\n\nVerified against: abcd1234\n\nBody\n"},
		{"replaced", "# T\n\nVerified against: abcdef12\n\nBody\n", "# T\n\nVerified against: abcd1234\n\nBody\n"},
		{"a title alone, no newline", "# T", "# T\n\nVerified against: abcd1234\n"},
		{"crlf kept", "# T\r\n\r\nBody\r\n", "# T\r\n\r\nVerified against: abcd1234\r\n\r\nBody\r\n"},
		{"prose before a chapter title", "intro\n\n## C\n\nBody\n", "intro\n\n## C\n\nVerified against: abcd1234\n\nBody\n"},
	}
	for _, c := range cases {
		got, err := SetStamp("a.md", []byte(c.in), "abcd1234")
		if err != nil || string(got) != c.want {
			t.Errorf("%s: SetStamp = %q, %v; want %q", c.name, got, err, c.want)
		}
		if again, err := SetStamp("a.md", got, "abcd1234"); err != nil || string(again) != string(got) {
			t.Errorf("%s: a second SetStamp changed the doc: %q, %v", c.name, again, err)
		}
	}
	if _, err := SetStamp("a.md", []byte("no heading here\n"), "X"); err == nil || !strings.Contains(err.Error(), "no title to stamp under") {
		t.Errorf("SetStamp on a doc with no heading: err = %v, want no title to stamp under", err)
	}
}

func TestCitedPath(t *testing.T) {
	cases := map[string]string{ // span -> path to resolve, "" = not a path citation
		"engine/foo.go":                 "engine/foo.go",
		"shingocore.yaml":               "shingocore.yaml",
		"store/bins/":                   "store/bins/",
		"engine/foo.go:42":              "engine/foo.go", // the line is repolint's other finding
		"docs/lanes.md#mouths":          "docs/lanes.md",
		"./engine/...":                  "./engine",
		"shingo-core/engine:TryLockFor": "shingo-core/engine",
		"README":                        "", // no slash, no extension
		"go test ./engine/":             "", // a command, not one path
		"/api/orders":                   "", // a route
		"/etc/shingo/shingocore.yaml":   "", // the host's
		"~/.ssh/config":                 "",
		"http://host:8083/x":            "",
		"mailto:a@b.c":                  "",
		"TestLock_Holds/a_case":         "", // a subtest: testcite's
		"*.go":                          "",
		"<module>/docs/":                "",
		"{core,edge}/x.go":              "",
		"$ROOT/x.sh":                    "",
		"engine/....go":                 "",
		"github.com/go-chi/chi/v5":      "", // another repo's module
		"gopkg.in/yaml.v3":              "",
		"text/plain":                    "", // media types
		"application/x-node-group":      "",
		".github/workflows/guards.yml":  ".github/workflows/guards.yml", // a dot-directory is ours
	}
	for span, want := range cases {
		got, ok := CitedPath(span)
		if got != want || ok != (want != "") {
			t.Errorf("CitedPath(%q) = %q, %v; want %q", span, got, ok, want)
		}
	}
}

func TestResolver(t *testing.T) {
	r := NewResolver([]string{
		"README.md",
		"docs/lanes.md",
		"docs/dev-env/sim.md",
		"shingo-core/engine/foo.go",
		"shingo-core/dispatch/binresolver/group_resolver.go",
		"shingo-edge/engine/changeover.go",
		"protocol/types.go",
		"scripts/gate.sh",
	})
	cited := []struct {
		doc, p string
		want   bool
	}{
		{"docs/dev-env/sim.md", "../lanes.md", true},             // relative to the doc
		{"docs/lanes.md", "docs/dev-env/sim.md", true},           // from the root
		{"docs/lanes.md", "scripts/gate.sh", true},               // from the root, outside the modules
		{"docs/lanes.md", "engine/foo.go", true},                 // module-relative
		{"docs/lanes.md", "binresolver/group_resolver.go", true}, // a suffix inside the modules
		{"docs/lanes.md", "dispatch/group_resolver.go", false},   // no such suffix: the file moved
		{"docs/lanes.md", "group_resolver.go", true},             // a bare basename
		{"docs/lanes.md", "gate.sh", true},                       // a basename outside the modules
		{"docs/lanes.md", "shingo-core/engine", true},            // a directory
		{"docs/lanes.md", "shingo-core/engine/", true},           // with its slash
		{"docs/lanes.md", "shingocore/engine", true},             // a Go import path
		{"docs/lanes.md", "shingo/protocol", true},               // the protocol module
		{"docs/lanes.md", "edge/engine/changeover.go", false},    // no suffix of shingo-edge's
		{"docs/lanes.md", "../deploy/journald.conf", false},      // climbs out and is not there
		{"docs/lanes.md", "engine/gone.go", false},
		{"docs/lanes.md", "AGENTS.md", false},
	}
	for _, c := range cited {
		if got := r.Cited(c.doc, c.p); got != c.want {
			t.Errorf("Cited(%q, %q) = %v, want %v", c.doc, c.p, got, c.want)
		}
	}
	links := []struct {
		doc, target string
		want        bool
	}{
		{"docs/dev-env/sim.md", "../lanes.md", true},
		{"README.md", "docs/lanes.md", true},
		{"README.md", "shingo-core/", true},
		{"README.md", "/docs/lanes.md", true},     // from the root, as GitHub reads it
		{"docs/lanes.md", "engine/foo.go", false}, // a link is relative to its doc only
		{"docs/lanes.md", "../../outside.md", false},
	}
	for _, c := range links {
		if got := r.Link(c.doc, c.target); got != c.want {
			t.Errorf("Link(%q, %q) = %v, want %v", c.doc, c.target, got, c.want)
		}
	}
}

func TestNames(t *testing.T) {
	cases := []struct {
		doc, cited, changed string
		want                bool
	}{
		{"docs/a.md", "shingo-core/engine/foo.go", "shingo-core/engine/foo.go", true},
		{"docs/a.md", "engine/foo.go", "shingo-core/engine/foo.go", true}, // module-relative
		{"docs/a.md", "foo.go", "shingo-core/engine/foo.go", true},        // basename
		{"docs/a.md", "../README.md", "README.md", true},                  // relative to the doc
		{"docs/a.md", "shingocore/engine", "shingo-core/engine", true},
		{"docs/a.md", "gine/foo.go", "shingo-core/engine/foo.go", false}, // not on a component boundary
		{"docs/a.md", "engine/bar.go", "shingo-core/engine/foo.go", false},
	}
	for _, c := range cases {
		if got := Names(c.doc, c.cited, c.changed); got != c.want {
			t.Errorf("Names(%q, %q, %q) = %v, want %v", c.doc, c.cited, c.changed, got, c.want)
		}
	}
}

func TestLineCites(t *testing.T) {
	got := LineCites("see engine/foo.go:42, `x.sql:7`, and a.md:3:9; not foo.go:bar or 10:30")
	want := []string{"engine/foo.go:42", "x.sql:7", "a.md:3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LineCites = %q, want %q", got, want)
	}
}

// TestImportRoots_MatchGoMod holds ImportRoots to the module line of each
// module's go.mod: a renamed module would otherwise leave its import paths
// unresolved in every doc.
func TestImportRoots_MatchGoMod(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..")
	got := map[string]string{}
	for _, m := range Modules {
		b, err := os.ReadFile(filepath.Join(root, m, "go.mod"))
		if err != nil {
			t.Fatalf("read %s/go.mod: %v", m, err)
		}
		for _, l := range strings.Split(string(b), "\n") {
			if f := strings.Fields(l); len(f) == 2 && f[0] == "module" {
				got[f[1]] = m
			}
		}
	}
	if !reflect.DeepEqual(got, ImportRoots) {
		t.Errorf("go.mod module paths %v, ImportRoots %v", got, ImportRoots)
	}
}

func TestLoad_TrackedInScopeOnly(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	dir := t.TempDir()
	files := map[string]string{
		"README.md":                         "# R\n",
		"docs/a.md":                         "# A\n",
		"docs/testing/population-ledger.md": "# L\n",
		"CHANGELOG.md":                      "# C\n",
		"notes.md":                          "# N\n",
	}
	for p, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "untracked.md"), []byte("# U\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ix, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, d := range ix.Docs {
		got = append(got, d.Path)
	}
	if want := []string{"README.md", "docs/a.md"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Load read %q, want %q: tracked, in scope, not exempt", got, want)
	}
	if len(ix.Tracked) != 5 {
		t.Errorf("Tracked = %q, want the 5 tracked files", ix.Tracked)
	}
}
