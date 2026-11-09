package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The tests below run the changelog check and the release against throwaway
// repos: a template shaped like shingo where it matters (the two migration
// registries, the two config packages, protocol/, a CHANGELOG.md), committed
// and tagged v0.1.0 once, then copied per test into a bare origin of its own
// and a working clone of that.

func run(t *testing.T, dir string, args ...string) string {
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

func read(t *testing.T, root, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// isolated keeps the user's git config out of the fixtures: no signing, no
// hooks, no line-ending conversion.
var isolated = []string{
	"-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid",
	"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false",
	"-c", "core.autocrlf=false", "-c", "core.hooksPath=.no-hooks",
	"-c", "init.defaultBranch=main",
}

// cloneConfig is isolated as `git clone --config` takes it.
func cloneConfig() []string {
	var out []string
	for i := 0; i < len(isolated); i += 2 {
		if isolated[i+1] != "init.defaultBranch=main" {
			out = append(out, "--config", isolated[i+1])
		}
	}
	return out
}

const (
	coreMigrations = "package store\n\nfunc migrationList() []migration {\n\treturn []migration{\n%s\t}\n}\n"
	edgeMigrations = "package store\n\nfunc edgeMigrations() []migrate.Migration {\n\treturn []migrate.Migration{\n%s\t}\n}\n"
	coreConfig     = "package config\n\ntype Config struct {\n\tWeb WebConfig `yaml:\"web\"`\n}\n\ntype WebConfig struct {\n\tPort int `yaml:\"port\"`\n}\n"
	edgeConfig     = "package config\n\ntype Config struct {\n\tStationUID string `yaml:\"station_uid\"`\n}\n"
	baseChangelog  = "# Changelog\n\nIntro.\n\n## Unreleased\n\n## v0.1.0 — 2026-01-02 — First\n\nDeploy: no migration.\n\n- The first release\n"
)

// coreAt is Core's registry declaring versions 1..n.
func coreAt(n int) string {
	var b strings.Builder
	for v := 1; v <= n; v++ {
		b.WriteString("\t\t{" + itoa(v) + ", \"m\", nil, nil},\n")
	}
	return strings.Replace(coreMigrations, "%s", b.String(), 1)
}

// edgeAt is the Edge's registry declaring versions 2..n.
func edgeAt(n int) string {
	var b strings.Builder
	for v := 2; v <= n; v++ {
		b.WriteString("\t\t{Version: " + itoa(v) + ", Name: \"m\"},\n")
	}
	return strings.Replace(edgeMigrations, "%s", b.String(), 1)
}

func itoa(n int) string { return strconv.Itoa(n) }

var (
	templateOnce sync.Once
	templateDir  string
	templateErr  error
)

func TestMain(m *testing.M) {
	// A test run from inside a git hook inherits GIT_DIR and its kin, which
	// would point every git call here at the repo the hook runs for, the real
	// one, instead of the fixture. None of them is wanted.
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(strings.ToUpper(k), "GIT_") {
			os.Unsetenv(k)
		}
	}
	code := m.Run()
	if templateDir != "" {
		os.RemoveAll(templateDir)
	}
	os.Exit(code)
}

// template builds, once, the bare repo every fixture copies.
func template(t *testing.T) string {
	t.Helper()
	templateOnce.Do(func() {
		dir, err := os.MkdirTemp("", "release-template-")
		if err != nil {
			templateErr = err
			return
		}
		templateDir = dir
		work := filepath.Join(dir, "work")
		g := func(args ...string) error {
			out, err := exec.Command("git", append(append([]string{"-C", work}, isolated...), args...)...).CombinedOutput()
			if err != nil {
				return errors.New(string(out))
			}
			return nil
		}
		if err := os.MkdirAll(work, 0o755); err != nil {
			templateErr = err
			return
		}
		files := map[string]string{
			"CHANGELOG.md":                    baseChangelog,
			"README.md":                       "A repo.\n",
			"protocol/go.mod":                 "module shingo/protocol\n",
			"protocol/wire.go":                "package protocol\n",
			"shingo-core/go.mod":              "module shingocore\n",
			"shingo-core/engine/engine.go":    "package engine\n",
			"shingo-core/store/migrations.go": coreAt(2),
			"shingo-core/config/config.go":    coreConfig,
			"shingo-edge/go.mod":              "module shingoedge\n",
			"shingo-edge/store/migrations.go": edgeAt(2),
			"shingo-edge/config/config.go":    edgeConfig,
		}
		for name, body := range files {
			p := filepath.Join(work, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				templateErr = err
				return
			}
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				templateErr = err
				return
			}
		}
		for _, step := range [][]string{
			{"init", "-q", "-b", "main"},
			{"add", "-A"},
			{"commit", "-q", "-m", "base"},
			{"tag", "-a", "v0.1.0", "-m", "v0.1.0"},
		} {
			if templateErr = g(step...); templateErr != nil {
				return
			}
		}
		out, err := exec.Command("git", "clone", "-q", "--no-hardlinks", "--bare", work, filepath.Join(dir, "template.git")).CombinedOutput()
		if err != nil {
			templateErr = errors.New(string(out))
		}
	})
	if templateErr != nil {
		t.Fatalf("template repo: %v", templateErr)
	}
	return filepath.Join(templateDir, "template.git")
}

// clone gives a test its own origin, a bare copy of the template, and a
// working clone of that on main at v0.1.0.
func clone(t *testing.T) (work, origin string) {
	t.Helper()
	tpl := template(t)
	dir := t.TempDir()
	origin, work = filepath.Join(dir, "origin.git"), filepath.Join(dir, "work")
	if out, err := exec.Command("git", "clone", "-q", "--no-hardlinks", "--bare", tpl, origin).CombinedOutput(); err != nil {
		t.Fatalf("clone origin: %v\n%s", err, out)
	}
	return cloneOf(t, origin, work), origin
}

// cloneOf makes a working clone of origin at dir.
func cloneOf(t *testing.T, origin, dir string) string {
	t.Helper()
	args := append([]string{"clone", "-q", "--no-hardlinks"}, cloneConfig()...)
	if out, err := exec.Command("git", append(args, origin, dir)...).CombinedOutput(); err != nil {
		t.Fatalf("clone work: %v\n%s", err, out)
	}
	return dir
}

// commitPush commits files and pushes main.
func commitPush(t *testing.T, work string, files map[string]string) {
	t.Helper()
	write(t, work, files)
	run(t, work, "add", "-A")
	run(t, work, "commit", "-q", "-m", "change")
	run(t, work, "push", "-q", "origin", "main")
}

func withUnreleased(body string) string {
	return strings.Replace(baseChangelog, "## Unreleased\n", "## Unreleased\n\n"+body+"\n", 1)
}

// ── The changelog check ─────────────────────────────────────────────────

func check(t *testing.T, root string) *CheckResult {
	t.Helper()
	r, err := CheckChangelog(root)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func wantProblems(t *testing.T, r *CheckResult, want ...string) {
	t.Helper()
	if len(r.Problems) != len(want) {
		t.Fatalf("problems %q, want %d: %q", r.Problems, len(want), want)
	}
	for i := range want {
		if !strings.Contains(r.Problems[i], want[i]) {
			t.Errorf("problem %q, want it to say %q", r.Problems[i], want[i])
		}
	}
	var out bytes.Buffer
	if ok := r.Print(&out); ok == (len(want) > 0) {
		t.Errorf("Print says ok=%v with problems %q:\n%s", ok, r.Problems, out.String())
	}
}

func TestCheckChangelog_NoTagPasses(t *testing.T) {
	t.Parallel()
	work, _ := clone(t)
	run(t, work, "tag", "-d", "v0.1.0")
	write(t, work, map[string]string{"shingo-core/engine/engine.go": "package engine\n\nvar X = 1\n"})
	r := check(t, work)
	wantProblems(t, r)
	var out bytes.Buffer
	r.Print(&out)
	if r.Tag != "" || !strings.Contains(out.String(), "no v* tag") {
		t.Errorf("with no tag: Tag %q, line %q", r.Tag, out.String())
	}
}

func TestCheckChangelog_OnlyNonSourceChangesNeedNoEntry(t *testing.T) {
	t.Parallel()
	work, _ := clone(t)
	write(t, work, map[string]string{
		"README.md":                         "Edited.\n",
		"shingo-core/engine/engine_test.go": "package engine\n",
		"shingo-core/README.md":             "A module.\n",
		"docs/x.md":                         "A doc.\n",
	})
	r := check(t, work)
	wantProblems(t, r)
	if len(r.Source) != 0 {
		t.Errorf("source %q, want none", r.Source)
	}
}

func TestCheckChangelog_ASourceChangeNeedsADeployLine(t *testing.T) {
	t.Parallel()
	work, _ := clone(t)
	// Uncommitted on purpose: the check reads the working tree, as the gate does.
	write(t, work, map[string]string{"shingo-core/engine/engine.go": "package engine\n\nvar X = 1\n"})
	wantProblems(t, check(t, work), "1 source files changed since v0.1.0 (shingo-core/engine/engine.go) and `## Unreleased` is empty")

	write(t, work, map[string]string{ChangelogPath: withUnreleased("- A change, and no Deploy line")})
	wantProblems(t, check(t, work), "opens with \"- A change, and no Deploy line\", not a Deploy: line")

	write(t, work, map[string]string{ChangelogPath: withUnreleased("Deploy: Core only.\n\n- A change")})
	wantProblems(t, check(t, work), "no migration was added since v0.1.0, and the Deploy line does not say `no migration`")

	write(t, work, map[string]string{ChangelogPath: withUnreleased("Deploy: Core only; no migration.\n\n- A change")})
	wantProblems(t, check(t, work))

	write(t, work, map[string]string{ChangelogPath: strings.Replace(baseChangelog, "## Unreleased\n", "", 1)})
	wantProblems(t, check(t, work), "no `## Unreleased` section")
}

func TestCheckChangelog_TheDeployLineNamesEveryAddedMigration(t *testing.T) {
	t.Parallel()
	work, _ := clone(t)
	write(t, work, map[string]string{
		"shingo-core/store/migrations.go": coreAt(3),
		"shingo-edge/store/migrations.go": edgeAt(4),
	})
	cases := []struct {
		deploy string
		want   []string
	}{
		{"Deploy: Core v3, Edge v3–v4; Core first.", nil},
		{"Deploy: Core v3 and Edge v3-4.", nil},
		{"Deploy: Core v3; Edge v3.", []string{"does not name Edge v4, added since v0.1.0"}},
		{"Deploy: Core v2–v3; Edge v3–v4.", []string{"names Core v2, which no migration added since v0.1.0 declares"}},
		{"Deploy: no migration.", []string{"does not name Core v3", "does not name Edge v3–v4"}},
		{"Deploy: v3 first.", []string{"names v3 before saying whose it is"}},
	}
	for _, c := range cases {
		write(t, work, map[string]string{ChangelogPath: withUnreleased(c.deploy + "\n\n- A change")})
		r := check(t, work)
		if len(r.Problems) != len(c.want) {
			t.Errorf("%q: problems %q, want %q", c.deploy, r.Problems, c.want)
			continue
		}
		for i := range c.want {
			if !strings.Contains(r.Problems[i], c.want[i]) {
				t.Errorf("%q: problem %q, want it to say %q", c.deploy, r.Problems[i], c.want[i])
			}
		}
	}
}

func TestCheckChangelog_AMovedRegistryIsAnError(t *testing.T) {
	t.Parallel()
	work, _ := clone(t)
	if err := os.Remove(filepath.Join(work, "shingo-edge", "store", "migrations.go")); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckChangelog(work); err == nil || !strings.Contains(err.Error(), "the registry moved") {
		t.Errorf("CheckChangelog with the Edge registry gone: %v, want an error naming the move", err)
	}
}

// ── The release ─────────────────────────────────────────────────────────

// newRelease is a release of version in work whose gate passes, counting its calls.
func newRelease(work, version string) (*Release, *int, *bytes.Buffer) {
	calls := new(int)
	out := new(bytes.Buffer)
	return &Release{
		Root: work, Version: version, Title: "Second", Out: out,
		Gate: func(string) error { *calls++; return nil },
		Now:  func() time.Time { return time.Date(2026, 11, 9, 12, 0, 0, 0, time.Local) },
	}, calls, out
}

// state is what a refused release must leave as it was. localTags are the
// clone's tags origin does not have: the release's fetch may bring origin's
// in, and must not make one of its own.
type state struct{ head, localTags, changelog, originHead, originTags string }

func snapshot(t *testing.T, work, origin string) state {
	t.Helper()
	// One show-ref per repo: a git process costs a quarter second on Windows.
	refs := func(dir string) map[string]string {
		m := map[string]string{}
		for _, line := range strings.Split(run(t, dir, "show-ref", "--head"), "\n") {
			if sha, ref, ok := strings.Cut(line, " "); ok {
				m[ref] = sha
			}
		}
		return m
	}
	local, remote := refs(work), refs(origin)
	var localTags, originTags []string
	for ref, sha := range remote {
		if strings.HasPrefix(ref, "refs/tags/") {
			originTags = append(originTags, ref+"="+sha)
		}
	}
	for ref, sha := range local {
		if strings.HasPrefix(ref, "refs/tags/") && remote[ref] != sha {
			localTags = append(localTags, ref+"="+sha)
		}
	}
	sort.Strings(originTags)
	sort.Strings(localTags)
	return state{
		head:       local["HEAD"],
		localTags:  strings.Join(localTags, " "),
		changelog:  read(t, work, ChangelogPath),
		originHead: remote["refs/heads/main"],
		originTags: strings.Join(originTags, " "),
	}
}

// refused runs r, wanting a refusal saying want, the gate run gates times and
// nothing written.
func refused(t *testing.T, r *Release, calls *int, gates int, origin, want string) {
	t.Helper()
	before := snapshot(t, r.Root, origin)
	err := r.Run()
	if err == nil || !isRefusal(err) || !strings.Contains(err.Error(), want) {
		t.Fatalf("Run: %v, want a refusal saying %q", err, want)
	}
	if *calls != gates {
		t.Errorf("the gate ran %d times, want %d", *calls, gates)
	}
	if after := snapshot(t, r.Root, origin); after != before {
		t.Errorf("a refused release changed the repo:\nbefore %+v\nafter  %+v", before, after)
	}
}

func TestRelease_RefusesAMalformedVersion(t *testing.T) {
	t.Parallel()
	// "" is not here: an empty VERSION asks for the computed one.
	for _, v := range []string{"0.1.1", "v0.1", "v0.01.1", "v0.1.1-rc1", "v0.1.1 "} {
		r, calls, _ := newRelease(t.TempDir(), v)
		err := r.Run()
		if err == nil || !isRefusal(err) || !strings.Contains(err.Error(), "is not vMAJOR.MINOR.PATCH") {
			t.Errorf("VERSION %q: %v, want a refusal of its shape", v, err)
		}
		if *calls != 0 {
			t.Errorf("VERSION %q: the gate ran", v)
		}
	}
}

func TestRelease_RefusesOffMain(t *testing.T) {
	t.Parallel()
	work, origin := clone(t)
	run(t, work, "checkout", "-q", "-b", "topic")
	r, calls, _ := newRelease(work, "v0.1.1")
	refused(t, r, calls, 0, origin, `on branch "topic"; a release is cut from main`)

	run(t, work, "checkout", "-q", "--detach", "main")
	r, calls, _ = newRelease(work, "v0.1.1")
	refused(t, r, calls, 0, origin, "HEAD is detached")
}

func TestRelease_RefusesADirtyWorktree(t *testing.T) {
	t.Parallel()
	work, origin := clone(t)
	write(t, work, map[string]string{"stray.txt": "x\n"})
	r, calls, _ := newRelease(work, "v0.1.1")
	refused(t, r, calls, 0, origin, "the worktree is not clean:\n  ?? stray.txt")
}

func TestRelease_RefusesAHeadThatIsNotOriginMain(t *testing.T) {
	t.Parallel()
	work, origin := clone(t)
	write(t, work, map[string]string{"README.md": "Local only.\n"})
	run(t, work, "commit", "-q", "-am", "unpushed")
	r, calls, _ := newRelease(work, "v0.1.1")
	refused(t, r, calls, 0, origin, "is not origin/main")

	// The other way: origin moved on, and only a fetch shows it.
	run(t, work, "reset", "-q", "--hard", "HEAD~1")
	other := cloneOf(t, origin, filepath.Join(t.TempDir(), "other"))
	commitPush(t, other, map[string]string{"README.md": "Pushed elsewhere.\n"})
	r, calls, _ = newRelease(work, "v0.1.1")
	refused(t, r, calls, 0, origin, "is not origin/main")
}

func TestRelease_RefusesATagThatExists(t *testing.T) {
	t.Parallel()
	work, origin := clone(t)
	run(t, origin, "tag", "v0.1.1", "main")
	r, calls, _ := newRelease(work, "v0.1.1")
	refused(t, r, calls, 0, origin, "tag v0.1.1 already exists")
}

func TestRelease_RefusesARedChangelogOrAnEmptyOne(t *testing.T) {
	t.Parallel()
	work, origin := clone(t)
	commitPush(t, work, map[string]string{"shingo-core/engine/engine.go": "package engine\n\nvar X = 1\n"})
	r, calls, _ := newRelease(work, "v0.1.1")
	refused(t, r, calls, 0, origin, "the changelog check fails:\n  - 1 source files changed since v0.1.0")

	work, origin = clone(t)
	commitPush(t, work, map[string]string{"README.md": "Docs only.\n"})
	r, calls, _ = newRelease(work, "v0.1.1")
	refused(t, r, calls, 0, origin, "`## Unreleased` in CHANGELOG.md is empty: there is nothing to release")
}

func TestRelease_RefusesAVersionBelowWhatTheChangesNeed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		files  map[string]string
		reason string
	}{
		{"migration", map[string]string{
			"shingo-core/store/migrations.go": coreAt(3),
			ChangelogPath:                     withUnreleased("Deploy: Core v3.\n\n- A column"),
		}, "Core adds migrations v3"},
		{"protocol", map[string]string{
			"protocol/wire.go": "package protocol\n\ntype Msg struct{}\n",
			ChangelogPath:      withUnreleased("Deploy: no migration.\n\n- A message"),
		}, "protocol/ source changed (protocol/wire.go)"},
		{"config key added", map[string]string{
			"shingo-core/config/config.go": strings.Replace(coreConfig, "\tPort int `yaml:\"port\"`\n", "\tPort int `yaml:\"port\"`\n\tHost string `yaml:\"host\"`\n", 1),
			ChangelogPath:                  withUnreleased("Deploy: no migration.\n\n- A setting"),
		}, "Core config keys added: web.host"},
		{"config key removed", map[string]string{
			"shingo-edge/config/config.go": "package config\n\ntype Config struct{}\n",
			ChangelogPath:                  withUnreleased("Deploy: no migration.\n\n- A setting gone"),
		}, "Edge config keys removed: station_uid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			work, origin := clone(t)
			commitPush(t, work, c.files)
			r, calls, out := newRelease(work, "v0.1.1")
			refused(t, r, calls, 0, origin, "v0.1.1 is below v0.2.0, the least MINOR allows after v0.1.0")
			if !strings.Contains(out.String(), c.reason) {
				t.Errorf("the release did not say why it is MINOR (%q):\n%s", c.reason, out.String())
			}
		})
	}
}

func TestRelease_RefusesARedGateOrOneThatDirtiesTheTree(t *testing.T) {
	t.Parallel()
	work, origin := clone(t)
	commitPush(t, work, map[string]string{
		"shingo-core/engine/engine.go": "package engine\n\nvar X = 1\n",
		ChangelogPath:                  withUnreleased("Deploy: no migration.\n\n- A fix"),
	})
	r, calls, _ := newRelease(work, "v0.1.1")
	r.Gate = func(string) error { *calls++; return errors.New("exit status 1") }
	refused(t, r, calls, 1, origin, "the gate failed (exit status 1); nothing was written")

	r, calls, _ = newRelease(work, "v0.1.1")
	r.Gate = func(root string) error {
		*calls++
		return os.WriteFile(filepath.Join(root, "gate-left-this.txt"), []byte("x"), 0o644)
	}
	err := r.Run()
	if err == nil || !strings.Contains(err.Error(), "after the gate: the worktree is not clean") {
		t.Fatalf("Run with a gate that writes a file: %v", err)
	}
	if run(t, work, "tag", "--list", "v0.1.1") != "" {
		t.Error("a release refused after the gate tagged anyway")
	}
}

func TestRelease_CutsCommitsTagsAndPushes(t *testing.T) {
	t.Parallel()
	work, origin := clone(t)
	commitPush(t, work, map[string]string{
		"shingo-core/engine/engine.go": "package engine\n\nvar X = 1\n",
		ChangelogPath:                  withUnreleased("Deploy: no migration.\n\n- A fix"),
	})
	before := run(t, work, "rev-parse", "HEAD")
	r, calls, out := newRelease(work, "v0.1.1")
	if err := r.Run(); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	if *calls != 1 {
		t.Errorf("the gate ran %d times, want once", *calls)
	}
	if !strings.Contains(out.String(), "since v0.1.0: PATCH, so at least v0.1.1") {
		t.Errorf("the release did not say its bump:\n%s", out.String())
	}

	wantHead := "# Changelog\n\nIntro.\n\n## Unreleased\n\n## v0.1.1 — 2026-11-09 — Second\n\nDeploy: no migration.\n\n- A fix\n\n## v0.1.0"
	if got := read(t, work, ChangelogPath); !strings.HasPrefix(got, wantHead) {
		t.Errorf("CHANGELOG.md after the release:\n%s\nwant it to open:\n%s", got, wantHead)
	}
	head := run(t, work, "rev-parse", "HEAD")
	if parent := run(t, work, "rev-parse", "HEAD~1"); parent != before {
		t.Errorf("the release commit's parent is %s, want the released tip %s", parent, before)
	}
	if s := run(t, work, "log", "-1", "--format=%s"); s != "release: v0.1.1" {
		t.Errorf("release commit subject %q", s)
	}
	if files := run(t, work, "diff", "--name-only", "HEAD~1", "HEAD"); files != ChangelogPath {
		t.Errorf("the release commit touches %q, want CHANGELOG.md alone", files)
	}
	if kind := run(t, work, "cat-file", "-t", "v0.1.1"); kind != "tag" {
		t.Errorf("v0.1.1 is a %s, want an annotated tag", kind)
	}
	if msg := run(t, work, "tag", "-l", "--format=%(contents:subject)", "v0.1.1"); msg != "v0.1.1 — 2026-11-09 — Second" {
		t.Errorf("tag message %q", msg)
	}
	if got := run(t, work, "rev-parse", "v0.1.1^{commit}"); got != head {
		t.Errorf("v0.1.1 points at %s, want the release commit %s", got, head)
	}
	if got := run(t, origin, "rev-parse", "main"); got != head {
		t.Errorf("origin main is %s, want the release commit %s", got, head)
	}
	if got := run(t, origin, "rev-parse", "v0.1.1^{commit}"); got != head {
		t.Errorf("origin's v0.1.1 is %s, want the release commit %s", got, head)
	}
	if n, err := Notes(read(t, work, ChangelogPath), "v0.1.1"); err != nil || n != "Deploy: no migration.\n\n- A fix\n" {
		t.Errorf("notes for the cut release: %q, %v", n, err)
	}
	// Released: the check holds an empty Unreleased to nothing again.
	wantProblems(t, check(t, work))
}

// ── The plan, the computed version, -ci ─────────────────────────────────

// plan is PlanRelease at work, wanting it to leave the repo as it was.
func plan(t *testing.T, work, origin string) *Plan {
	t.Helper()
	before := snapshot(t, work, origin)
	p, err := PlanRelease(work)
	if err != nil {
		t.Fatal(err)
	}
	if after := snapshot(t, work, origin); after != before {
		t.Errorf("PlanRelease changed the repo:\nbefore %+v\nafter  %+v", before, after)
	}
	return p
}

func TestPlan_SaysWhetherAndWhatToRelease(t *testing.T) {
	t.Parallel()
	work, origin := clone(t)
	if line := plan(t, work, origin).Line(); line != "release=no reason=`## Unreleased` is empty" {
		t.Errorf("an empty Unreleased plans %q", line)
	}

	commitPush(t, work, map[string]string{
		"shingo-core/engine/engine.go": "package engine\n\nvar X = 1\n",
		ChangelogPath:                  withUnreleased("Deploy: no migration.\n\n- A fix"),
	})
	want := "release=yes version=v0.1.1 bump=patch reason=no migration, no protocol/ source change, no config key added or removed"
	if line := plan(t, work, origin).Line(); line != want {
		t.Errorf("a fix plans %q, want %q", line, want)
	}

	commitPush(t, work, map[string]string{
		"shingo-core/store/migrations.go": coreAt(3),
		ChangelogPath:                     withUnreleased("Deploy: Core v3.\n\n- A column"),
	})
	p := plan(t, work, origin)
	if !strings.HasPrefix(p.Line(), "release=yes version=v0.2.0 bump=minor reason=Core adds migrations v3") {
		t.Errorf("a migration plans %q", p.Line())
	}
	if len(p.Problems) != 0 {
		t.Errorf("problems %q", p.Problems)
	}
}

func TestRelease_ComputesTheVersionWhenNoneIsGiven(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, want string
		files      map[string]string
	}{
		{"patch", "v0.1.1", map[string]string{
			"shingo-core/engine/engine.go": "package engine\n\nvar X = 1\n",
			ChangelogPath:                  withUnreleased("Deploy: no migration.\n\n- A fix"),
		}},
		{"minor", "v0.2.0", map[string]string{
			"shingo-core/store/migrations.go": coreAt(3),
			ChangelogPath:                     withUnreleased("Deploy: Core v3.\n\n- A column"),
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			work, origin := clone(t)
			commitPush(t, work, c.files)
			r, calls, out := newRelease(work, "")
			if err := r.Run(); err != nil {
				t.Fatalf("Run: %v\n%s", err, out.String())
			}
			if *calls != 1 {
				t.Errorf("the gate ran %d times, want once", *calls)
			}
			head := run(t, work, "rev-parse", "HEAD")
			if got := run(t, origin, "rev-parse", c.want+"^{commit}"); got != head {
				t.Errorf("origin's %s is %s, want the release commit %s", c.want, got, head)
			}
		})
	}
}

func TestRelease_CISkipsTheGateAndNothingElse(t *testing.T) {
	t.Parallel()
	work, origin := clone(t)
	commitPush(t, work, map[string]string{
		"shingo-core/engine/engine.go": "package engine\n\nvar X = 1\n",
		ChangelogPath:                  withUnreleased("Deploy: no migration.\n\n- A fix"),
	})
	// Every other refusal stands: one before the gate would have run, one after.
	write(t, work, map[string]string{"stray.txt": "x\n"})
	r, calls, _ := newRelease(work, "")
	r.CI = true
	refused(t, r, calls, 0, origin, "the worktree is not clean")
	if err := os.Remove(filepath.Join(work, "stray.txt")); err != nil {
		t.Fatal(err)
	}
	r, calls, _ = newRelease(work, "v0.1.0")
	r.CI = true
	refused(t, r, calls, 0, origin, "tag v0.1.0 already exists")

	r, calls, out := newRelease(work, "")
	r.CI = true
	if err := r.Run(); err != nil {
		t.Fatalf("Run -ci: %v\n%s", err, out.String())
	}
	if *calls != 0 {
		t.Errorf("-ci ran the gate %d times", *calls)
	}
	if !strings.Contains(out.String(), "-ci: the workflow's gate jobs ran on") {
		t.Errorf("-ci did not say why it skipped the gate:\n%s", out.String())
	}
	head := run(t, work, "rev-parse", "HEAD")
	if got := run(t, origin, "rev-parse", "v0.1.1^{commit}"); got != head {
		t.Errorf("origin's v0.1.1 is %s, want the release commit %s", got, head)
	}
}

func TestRelease_ANonFastForwardPushIsRefusedNeverForced(t *testing.T) {
	t.Parallel()
	work, origin := clone(t)
	commitPush(t, work, map[string]string{
		"shingo-core/engine/engine.go": "package engine\n\nvar X = 1\n",
		ChangelogPath:                  withUnreleased("Deploy: no migration.\n\n- A fix"),
	})
	other := cloneOf(t, origin, filepath.Join(t.TempDir(), "other"))
	r, calls, out := newRelease(work, "")
	r.CI = true
	r.beforePush = func() { commitPush(t, other, map[string]string{"README.md": "Pushed meanwhile.\n"}) }
	err := r.Run()
	if err == nil || !isRefusal(err) || !strings.Contains(err.Error(), "was not a fast-forward and origin refused it") {
		t.Fatalf("Run with a push landing meanwhile: %v\n%s", err, out.String())
	}
	if *calls != 0 {
		t.Errorf("-ci ran the gate")
	}
	if got, want := run(t, origin, "rev-parse", "main"), run(t, other, "rev-parse", "HEAD"); got != want {
		t.Errorf("origin main is %s, want the other push %s: the release forced its way in", got, want)
	}
	if tags := run(t, origin, "tag", "--list", "v0.1.1"); tags != "" {
		t.Errorf("origin has %q: the atomic push let the tag through without main", tags)
	}
}

// A doc or a test under protocol/ changes no binary: it is PATCH. docs-verify
// restamps protocol/migrate/README.md in every stream, so a rule that counted
// any protocol/ path would make every release MINOR.
func TestPlan_AProtocolDocOrTestIsPatch(t *testing.T) {
	t.Parallel()
	work, origin := clone(t)
	commitPush(t, work, map[string]string{
		"protocol/migrate/README.md": "# migrate\n\nVerified against: 0123abcd\n",
		"protocol/wire_test.go":      "package protocol\n",
		ChangelogPath:                withUnreleased("Deploy: no migration.\n\n- Docs"),
	})
	if line := plan(t, work, origin).Line(); !strings.HasPrefix(line, "release=yes version=v0.1.1 bump=patch") {
		t.Errorf("a protocol/ doc and test plan %q, want PATCH", line)
	}
}
