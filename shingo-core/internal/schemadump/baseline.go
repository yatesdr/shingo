package schemadump

import (
	"fmt"
	"os/exec"
	"strings"

	"shingocore/store"
)

// Vintage is one way a plant's database was made, rebuilt for the
// convergence test from git: by the code the plant ran, not by a copy of a
// plant. No plant access, nothing to keep in sync, and it re-runs in CI.
type Vintage struct {
	// Name labels the subtest.
	Name string
	// DDLRev, when set, is the git revision whose baseline DDL the database is
	// born on (BaselineFromGit), applied raw before any release boots it.
	DDLRev string
	// BuiltBy, when set, is the release whose own store.Open makes the
	// database (BuildRelease), on an empty database or on DDLRev's.
	BuiltBy string
	// Why says what the vintage stands for. A convergence failure is only
	// diagnosable if you know what the vintage was chosen to prove.
	Why string
	// Current marks BuiltBy as a release past the retired chain: this build
	// boots its database as it stands, with no refusal and no walk.
	Current bool
	// Drift, when set, is SQL applied after BuiltBy's boot: leftovers a plant's
	// database carries and a new one does not, which this build's migrations
	// must remove (DriftA, DriftB).
	Drift string
}

// Vintages are the databases the convergence test brings up with this build.
// Each one not at the retired chain's head is refused first, by design, and
// walked up by store.RetiredChainRelease, the release that carries the chain.
var Vintages = []Vintage{
	{
		Name:    "built-by-v0.11.0",
		BuiltBy: "v0.11.0",
		Why:     "a database built by v0.11.0 (schema v178, 2026-11-17), as a plant on that release has",
	},
	{
		Name:    "built-by-v0.13.2",
		BuiltBy: "v0.13.2",
		Why:     "a database built by v0.13.2 (schema v180, 2026-11-18), as a plant on that release has",
	},
	{
		Name:    "built-by-v0.16.0",
		BuiltBy: "v0.16.0",
		Why:     "a database built by v0.16.0 (schema v183, 2026-11-20), as both plants had when the chain retired",
	},
	{
		Name:    "built-by-" + store.RetiredChainRelease,
		BuiltBy: store.RetiredChainRelease,
		Why:     "a database built by the retired chain's own release: the replay's end state, which this build boots as it stands",
	},
	{
		Name:   "born-on-v0.0.14-ddl",
		DDLRev: "v0.0.14",
		Why: "born on the oldest tagged baseline DDL still readable (v0.0.14, 2026-03-21, at the pre-Phase-6.0a path), " +
			"no chain run on it, then walked up by " + store.RetiredChainRelease + "'s replay: the deepest upgrade there is",
	},
	{
		Name:    "v0.18.0-with-drift-a",
		BuiltBy: "v0.18.0",
		Current: true,
		Drift:   DriftA,
		Why:     "a v0.18.0 database (schema v185) carrying the leftovers its boot named at one plant: a payloads table an old copy rebuilt, nodes columns still integer, two stray tables",
	},
	{
		Name:    "v0.18.0-with-drift-b",
		BuiltBy: "v0.18.0",
		Current: true,
		Drift:   DriftB,
		Why:     "a v0.18.0 database (schema v185) carrying the leftovers its boot named at the other plant: a hand-added orders column and table",
	},
}

// baselinePaths are the places the baseline DDL has lived, newest first.
// Phase 6.0a moved it from store/schema_postgres.go to store/schema/
// postgres_ddl.go, so reaching a pre-6.0a vintage means trying both.
var baselinePaths = []string{
	"shingo-core/store/schema/postgres_ddl.go",
	"shingo-core/store/schema_postgres.go",
}

// BaselineFromGit reads a historical baseline DDL out of the repository at
// rev and returns the raw SQL.
//
// repoRoot is the directory to run git in — any path inside the work tree.
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
// declaration. The const has been named both postgresDDL and schemaPostgres,
// so match on the shape — a const whose value is a backtick-quoted literal
// containing CREATE TABLE — rather than on the name.
//
// Scanning until the DDL is found, rather than taking the first const in the
// file, is what makes this robust across historical revisions: the first const
// only happens to be the DDL in the vintages currently pinned, and a revision
// that declares anything else ahead of it would fail with "wrong const" on a
// file that does contain the schema. Edge's copy already scanned; this is that
// loop (shingo-edge/internal/schemadump/baseline.go).
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
	_, ok := resolveCommit(repoRoot, rev)
	return ok
}

// resolveCommit is the commit id rev names, and whether git could resolve it.
func resolveCommit(repoRoot, rev string) (string, bool) {
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}
