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
	Root    string
	Version string
	Title   string
	Out     io.Writer
	Gate    func(root string) error
	Now     func() time.Time
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
	v, ok := ParseVersion(r.Version)
	if !ok {
		return refuse("VERSION %q is not vMAJOR.MINOR.PATCH (v0.1.0, v0.1.1, v1.0.0)", r.Version)
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
	if _, err := git(r.Root, "rev-parse", "-q", "--verify", "refs/tags/"+v.String()); err == nil {
		return refuse("tag %s already exists", v)
	}
	last, err := LastTag(r.Root)
	if err != nil {
		return err
	}
	check, err := checkSince(r.Root, last)
	if err != nil {
		return err
	}
	if len(check.Problems) > 0 {
		return refuse("the changelog check fails:\n  - %s", strings.Join(check.Problems, "\n  - "))
	}
	if len(check.Body) == 0 {
		return refuse("`## Unreleased` in %s is empty: there is nothing to release", ChangelogPath)
	}
	if last == "" {
		r.say("no earlier v* tag: %s is the first release", v)
	} else {
		lastV, _ := ParseVersion(last)
		bump, reasons, err := Required(r.Root, last, check.Paths, check.Added)
		if err != nil {
			return err
		}
		least := lastV.Next(bump)
		r.say("since %s: %s, so at least %s (%s)", last, bump, least, strings.Join(reasons, "; "))
		if v.Less(least) {
			return refuse("%s is below %s, the least %s allows after %s", v, least, bump, last)
		}
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
	return r.write(v)
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

// write is the release itself, once every check has passed.
func (r *Release) write(v Version) error {
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
	if _, err := git(r.Root, "push", "--atomic", "--quiet", "origin",
		"refs/heads/main:refs/heads/main", "refs/tags/"+v.String()+":refs/tags/"+v.String()); err != nil {
		return fmt.Errorf("push: %w\nThe release commit %s and tag %s are local only. Push them with "+
			"`git push --atomic origin main %s`, or undo with `git tag -d %s && git reset --hard origin/main`",
			err, short(sha), v, v, v)
	}
	r.say("%s tagged at %s and pushed with main; the Release workflow builds and publishes it", v, short(sha))
	r.say("heading: ## %s", heading)
	return nil
}

// Required is the least bump the changes since tag call for, and why. MINOR
// when a migration was added (a binary older than it cannot run the
// database), anything under protocol/ changed (the wire Core and Edge share),
// or a config yaml key was added or removed (a config file one version reads
// the other may refuse or ignore); PATCH otherwise. paths and added are
// ChangedPaths and AddedMigrations since tag.
func Required(root, tag string, paths []string, added map[string][]int) (Bump, []string, error) {
	var reasons []string
	for _, reg := range Registries {
		if vs := added[reg.Binary]; len(vs) > 0 {
			reasons = append(reasons, fmt.Sprintf("%s adds migrations %s", reg.Binary, FormatVersions(vs)))
		}
	}
	var proto []string
	for _, p := range paths {
		if strings.HasPrefix(p, "protocol/") {
			proto = append(proto, p)
		}
	}
	if len(proto) > 0 {
		reasons = append(reasons, fmt.Sprintf("protocol/ changed (%s)", sample(proto)))
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
	return Patch, []string{"no migration, no protocol/ change, no config key added or removed"}, nil
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
