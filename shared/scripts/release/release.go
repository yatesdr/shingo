package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Release cuts one release. Gate and Now are fields so a test stands in for
// the cert gate and the clock; main sets the real ones.
type Release struct {
	Root string
	// Version is the release to cut; "" cuts the least version the changes
	// since the last tag allow (Plan.Version).
	Version string
	Title   string
	// CI skips the cert gate, and only the gate: the auto-release workflow's
	// gate jobs ran on this same sha before its cut job starts. Every other
	// refusal stands.
	CI   bool
	Out  io.Writer
	Gate func(root string) error
	Now  func() time.Time

	// beforePush, when set, runs just before the push: a test's way to land a
	// push on origin between the last check and the release's own.
	beforePush func()
}

// Plan is what a release cut from the tree now would be.
type Plan struct {
	Last     string // the last v* tag, "" when there is none
	Version  Version
	Bump     Bump
	Reasons  []string
	Content  bool     // `## Unreleased` holds something to release
	Problems []string // the changelog check's findings
}

// PlanRelease reads the tree at root and writes nothing: the last tag, the
// least version and bump the changes since it allow and why (Required), and
// whether `## Unreleased` has content. With no v* tag the first release is
// v0.1.0.
func PlanRelease(root string) (*Plan, error) {
	last, err := LastTag(root)
	if err != nil {
		return nil, err
	}
	check, err := checkSince(root, last)
	if err != nil {
		return nil, err
	}
	p := &Plan{Last: last, Content: len(check.Body) > 0, Problems: check.Problems}
	if last == "" {
		p.Version, p.Bump, p.Reasons = Version{0, 1, 0}, Minor, []string{"no earlier v* tag: the first release"}
		return p, nil
	}
	lastV, _ := ParseVersion(last)
	if p.Bump, p.Reasons, err = Required(root, last, check.Paths, check.Added); err != nil {
		return nil, err
	}
	p.Version = lastV.Next(p.Bump)
	return p, nil
}

// Line is the plan as one machine-readable line, the first -plan prints:
// `release=yes version=vX.Y.Z bump=patch|minor reason=...` when `## Unreleased`
// has content, `release=no reason=...` when it has none. The reason is last
// because it holds spaces.
func (p *Plan) Line() string {
	if !p.Content {
		return "release=no reason=`## Unreleased` is empty"
	}
	return fmt.Sprintf("release=yes version=%s bump=%s reason=%s",
		p.Version, strings.ToLower(p.Bump.String()), strings.Join(p.Reasons, "; "))
}

// Refusal is a precondition the release does not meet. Nothing has been
// written when Run returns one.
type Refusal struct{ Msg string }

func (r Refusal) Error() string { return r.Msg }

func refuse(format string, args ...any) error { return Refusal{fmt.Sprintf(format, args...)} }

// GateCommand is the cert tier: every gate step, the docker suites whatever
// the diff.
var GateCommand = []string{"bash", "scripts/gate.sh", "full", "--unscoped"}

// RunGate runs GateCommand in root with its output on this process's.
func RunGate(root string) error {
	cmd := exec.Command(GateCommand[0], GateCommand[1:]...)
	cmd.Dir = root
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func (r *Release) say(format string, args ...any) {
	fmt.Fprintf(r.Out, "release: "+format+"\n", args...)
}

// Run checks every precondition, the cheap ones first so a wrong VERSION does
// not wait behind the gate, then writes the release: CHANGELOG.md's
// `## Unreleased` renamed to the version's heading under a fresh empty one,
// committed as `release: vX.Y.Z`, an annotated tag, and one atomic push of
// main and the tag.
func (r *Release) Run() error {
	var v Version
	if r.Version != "" {
		var ok bool
		if v, ok = ParseVersion(r.Version); !ok {
			return refuse("VERSION %q is not vMAJOR.MINOR.PATCH (v0.1.0, v0.1.1, v1.0.0)", r.Version)
		}
	}
	if strings.ContainsAny(r.Title, "\r\n") {
		return refuse("TITLE is one line")
	}
	top, err := TopLevel(r.Root)
	if err != nil {
		return err
	}
	r.Root = top
	branch, err := gitLine(r.Root, "symbolic-ref", "-q", "--short", "HEAD")
	if err != nil || branch == "" {
		return refuse("HEAD is detached; a release is cut from main")
	}
	if branch != "main" {
		return refuse("on branch %q; a release is cut from main", branch)
	}
	if err := r.clean(); err != nil {
		return err
	}
	if _, err := git(r.Root, "fetch", "--quiet", "--tags", "origin"); err != nil {
		return refuse("cannot fetch origin: %v", err)
	}
	head, err := gitLine(r.Root, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	upstream, err := gitLine(r.Root, "rev-parse", "--verify", "-q", "refs/remotes/origin/main")
	if err != nil || upstream == "" {
		return refuse("no origin/main to compare HEAD with")
	}
	if head != upstream {
		return refuse("HEAD %s is not origin/main %s: release what origin has (push or pull first)", short(head), short(upstream))
	}
	if r.Version != "" {
		if err := r.tagAbsent(v); err != nil {
			return err
		}
	}
	plan, err := PlanRelease(r.Root)
	if err != nil {
		return err
	}
	if len(plan.Problems) > 0 {
		return refuse("the changelog check fails:\n  - %s", strings.Join(plan.Problems, "\n  - "))
	}
	if !plan.Content {
		return refuse("`## Unreleased` in %s is empty: there is nothing to release", ChangelogPath)
	}
	if r.Version == "" {
		v = plan.Version
		if err := r.tagAbsent(v); err != nil {
			return err
		}
	}
	if plan.Last == "" {
		r.say("no earlier v* tag: %s is the first release", v)
	} else {
		r.say("since %s: %s, so at least %s (%s)", plan.Last, plan.Bump, plan.Version, strings.Join(plan.Reasons, "; "))
		if v.Less(plan.Version) {
			return refuse("%s is below %s, the least %s allows after %s", v, plan.Version, plan.Bump, plan.Last)
		}
	}
	if r.CI {
		r.say("-ci: the workflow's gate jobs ran on %s; the local cert gate is not run", short(head))
		return r.write(v, head)
	}
	r.say("running %s (the cert gate)", strings.Join(GateCommand, " "))
	if err := r.Gate(r.Root); err != nil {
		return refuse("the gate failed (%v); nothing was written", err)
	}
	if err := r.clean(); err != nil {
		return refuse("after the gate: %v", err)
	}
	if _, err := git(r.Root, "fetch", "--quiet", "origin"); err != nil {
		return refuse("cannot fetch origin after the gate: %v", err)
	}
	if now, _ := gitLine(r.Root, "rev-parse", "HEAD"); now != head {
		return refuse("HEAD moved during the gate (%s to %s); start again", short(head), short(now))
	}
	if now, _ := gitLine(r.Root, "rev-parse", "refs/remotes/origin/main"); now != head {
		return refuse("origin/main moved during the gate (%s to %s); start again", short(head), short(now))
	}
	return r.write(v, head)
}

// tagAbsent refuses a version that is already tagged.
func (r *Release) tagAbsent(v Version) error {
	if _, err := git(r.Root, "rev-parse", "-q", "--verify", "refs/tags/"+v.String()); err == nil {
		return refuse("tag %s already exists", v)
	}
	return nil
}

// clean refuses a worktree with any change, untracked files included: the
// release commit is to carry the changelog and nothing else.
func (r *Release) clean() error {
	out, err := gitLine(r.Root, "status", "--porcelain")
	if err != nil {
		return err
	}
	if out != "" {
		lines := strings.Split(out, "\n")
		if len(lines) > 10 {
			lines = append(lines[:10], fmt.Sprintf("... and %d more", len(lines)-10))
		}
		return refuse("the worktree is not clean:\n  %s", strings.Join(lines, "\n  "))
	}
	return nil
}

// write is the release itself, once every check has passed; base is the
// checked HEAD, which origin/main was.
func (r *Release) write(v Version, base string) error {
	p := filepath.Join(r.Root, ChangelogPath)
	text, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	heading := ReleaseHeading(v.String(), r.Now().Format("2006-01-02"), r.Title)
	cut, err := Cut(string(text), heading)
	if err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte(cut), 0o644); err != nil {
		return err
	}
	if _, err := git(r.Root, "add", "--", ChangelogPath); err != nil {
		return err
	}
	if _, err := git(r.Root, "commit", "-q", "-m", "release: "+v.String()); err != nil {
		return fmt.Errorf("commit: %w (CHANGELOG.md is rewritten and staged; `git checkout HEAD -- CHANGELOG.md` undoes it)", err)
	}
	if _, err := git(r.Root, "tag", "-a", v.String(), "-m", heading); err != nil {
		return fmt.Errorf("tag: %w (the release commit is local; `git reset --hard origin/main` undoes it)", err)
	}
	sha, _ := gitLine(r.Root, "rev-parse", "HEAD")
	if r.beforePush != nil {
		r.beforePush()
	}
	// Atomic and never forced: if a push landed on main since the checks, the
	// branch update is not a fast-forward, origin refuses both refs, and
	// nothing of this release reaches origin.
	if _, err := git(r.Root, "push", "--atomic", "--quiet", "origin",
		"refs/heads/main:refs/heads/main", "refs/tags/"+v.String()+":refs/tags/"+v.String()); err != nil {
		if _, ferr := git(r.Root, "fetch", "--quiet", "origin"); ferr == nil {
			if now, _ := gitLine(r.Root, "rev-parse", "refs/remotes/origin/main"); now != "" && now != base {
				return refuse("origin/main moved to %s after the checks, so the push was not a fast-forward and origin "+
					"refused it; nothing reached origin. The next release carries both changes. Locally: "+
					"`git tag -d %s && git reset --hard origin/main` drops this attempt", short(now), v)
			}
		}
		return fmt.Errorf("push: %w\nThe release commit %s and tag %s are local only. Push them with "+
			"`git push --atomic origin main %s`, or undo with `git tag -d %s && git reset --hard origin/main`",
			err, short(sha), v, v, v)
	}
	r.say("%s tagged at %s and pushed with main", v, short(sha))
	r.say("heading: ## %s", heading)
	return nil
}

// Required is the least bump the changes since tag call for, and why. MINOR
// when a migration was added (a binary older than it cannot run the
// database), protocol/'s source changed (the wire Core and Edge share), or a
// config yaml key was added or removed (a config file one version reads the
// other may refuse or ignore); PATCH otherwise. paths and added are
// ChangedPaths and AddedMigrations since tag.
//
// "Source" is IsSource, as the changelog check reads it: a doc or a test under
// protocol/ changes no binary, and every stream restamps the docs
// (docs-verify), protocol/migrate/README.md among them, so counting any path
// would make every release MINOR and tell a plant to migrate when nothing on
// the wire moved.
func Required(root, tag string, paths []string, added map[string][]int) (Bump, []string, error) {
	var reasons []string
	for _, reg := range Registries {
		if vs := added[reg.Binary]; len(vs) > 0 {
			reasons = append(reasons, fmt.Sprintf("%s adds migrations %s", reg.Binary, FormatVersions(vs)))
		}
	}
	var proto []string
	for _, p := range paths {
		if strings.HasPrefix(p, "protocol/") && IsSource(p) {
			proto = append(proto, p)
		}
	}
	if len(proto) > 0 {
		reasons = append(reasons, fmt.Sprintf("protocol/ source changed (%s)", sample(proto)))
	}
	delta, err := ConfigDelta(root, tag)
	if err != nil {
		return Patch, nil, err
	}
	for _, pkg := range ConfigPackages {
		d := delta[pkg.Binary]
		if len(d.Added) > 0 {
			reasons = append(reasons, fmt.Sprintf("%s config keys added: %s", pkg.Binary, sample(d.Added)))
		}
		if len(d.Removed) > 0 {
			reasons = append(reasons, fmt.Sprintf("%s config keys removed: %s", pkg.Binary, sample(d.Removed)))
		}
	}
	if len(reasons) > 0 {
		return Minor, reasons, nil
	}
	return Patch, []string{"no migration, no protocol/ source change, no config key added or removed"}, nil
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// isRefusal reports whether err is a Refusal.
func isRefusal(err error) bool {
	var r Refusal
	return errors.As(err, &r)
}
