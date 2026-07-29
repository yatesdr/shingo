package schemadump

import (
	"fmt"
	"os/exec"
	"strings"
)

// Vintage is one historical baseline to build an "aged" edge database from.
// Sourced from git — no plant access, nothing to keep in sync, re-runnable in
// CI. See the Core package for the full rationale.
type Vintage struct {
	Rev string
	Why string
}

// Vintages are the pinned baselines the convergence test builds.
var Vintages = []Vintage{
	{
		Rev: "06da8cdf^",
		Why: "before the structured queue codes landed (06da8cdf, 2026-07-19) — the most recent " +
			"shape change to reach a fresh edge through the baseline CREATE TABLE",
	},
	{
		Rev: "f446fda7",
		Why: "the oldest revision at which the baseline lives at its current path (2026-04-29) — " +
			"the deepest upgrade this can build without also chasing the Phase-6.0b file move",
	},
}

// baselinePaths are the places Edge's baseline DDL has lived, newest first.
var baselinePaths = []string{
	"shingo-edge/store/schema/sqlite_ddl.go",
	"shingo-edge/store/schema.go",
}

// BaselineFromGit reads a historical baseline DDL out of the repository at rev.
func BaselineFromGit(repoRoot, rev string) (string, error) {
	var errs []string
	for _, p := range baselinePaths {
		cmd := exec.Command("git", "show", rev+":"+p)
		cmd.Dir = repoRoot
		out, err := cmd.Output()
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", p, err))
			continue
		}
		ddl, err := extractDDLConst(string(out))
		if err != nil {
			return "", fmt.Errorf("%s at %s: %w", p, rev, err)
		}
		return ddl, nil
	}
	// A revision that is entirely absent is a DIFFERENT failure from a
	// revision whose file cannot be parsed, and conflating them is what
	// made the CI failure opaque.
	if !revisionExists(repoRoot, rev) {
		return "", fmt.Errorf("%s", missingRevisionHint(rev))
	}
	return "", fmt.Errorf("no baseline DDL found at %s (tried %s)", rev, strings.Join(errs, "; "))
}

// extractDDLConst pulls the raw-string body out of the baseline const
// declaration. Match on the shape — a const whose value is a backtick-quoted
// literal containing CREATE TABLE — rather than on the name, which has moved.
func extractDDLConst(src string) (string, error) {
	rest := src
	for {
		idx := strings.Index(rest, "const ")
		if idx < 0 {
			return "", fmt.Errorf("no const with a CREATE TABLE raw string")
		}
		rest = rest[idx+len("const "):]
		open := strings.Index(rest, "`")
		if open < 0 {
			continue
		}
		closeIdx := strings.Index(rest[open+1:], "`")
		if closeIdx < 0 {
			return "", fmt.Errorf("unterminated raw string literal")
		}
		ddl := rest[open+1 : open+1+closeIdx]
		if strings.Contains(strings.ToUpper(ddl), "CREATE TABLE") {
			return ddl, nil
		}
		rest = rest[open+1+closeIdx:]
	}
}

// missingRevisionHint recognises the shallow-clone failure and says what it
// means.
//
// It cost a full local reproduction to work out that "exit status 128" was a
// CI checkout-depth problem rather than a real convergence failure — the error
// named the file it could not read and said nothing about why. A test whose
// failure mode needs an investigation to interpret is only half a test.
func missingRevisionHint(rev string) string {
	return fmt.Sprintf(
		"revision %s is not present in this clone.\n"+
			"If this is CI: actions/checkout defaults to a SHALLOW clone, where older\n"+
			"commits and tags do not exist, so the workflow needs fetch-depth: 0.\n"+
			"This is NOT a convergence failure — the test could not run at all.", rev)
}

// revisionExists reports whether git can resolve rev at all.
func revisionExists(repoRoot, rev string) bool {
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	cmd.Dir = repoRoot
	return cmd.Run() == nil
}
