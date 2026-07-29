package schemadump

import (
	"fmt"
	"os/exec"
	"strings"
)

// Vintage is one historical baseline to build an "aged" database from.
//
// Sourced from GIT, not from a plant: no plant access, nothing to keep in
// sync, and it re-runs in CI. Pinned by commit SHA rather than tag because a
// SHA cannot move, and because the shape changes worth straddling all landed
// after the last tag.
type Vintage struct {
	// Rev is the git revision to read the baseline out of.
	Rev string
	// Why says what shape change this vintage sits on the far side of. A
	// convergence failure is only diagnosable if you know what the vintage was
	// chosen to prove.
	Why string
}

// Vintages are the pinned baselines the convergence test builds.
//
// Each straddles a shape change that reached a fresh database through the
// baseline CREATE TABLE — which is precisely the change a plant database does
// NOT get from the baseline, because CREATE TABLE IF NOT EXISTS no-ops on a
// table that already exists.
var Vintages = []Vintage{
	{
		Rev: "06da8cdf^",
		Why: "before queue_code/queue_cause entered the baseline (06da8cdf, 2026-07-19) — " +
			"both are in migrateAddBaselineColumns, so this vintage proves that list carries them",
	},
	{
		Rev: "2342a216^",
		Why: "before lineside_buckets.core_node_name entered the baseline (2342a216, 2026-05-21) — " +
			"the v21 rename, and the reason that column is in migrateAddBaselineColumns",
	},
	{
		Rev: "v0.0.14",
		Why: "the oldest tagged release still readable (2026-03-21) — predates lineside_buckets " +
			"entirely and lives at the pre-Phase-6.0a path, so it exercises the deepest upgrade we can build",
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
	return "", fmt.Errorf("no baseline DDL found at %s (tried %s)", rev, strings.Join(errs, "; "))
}

// extractDDLConst pulls the raw-string body out of the baseline const
// declaration. The const has been named both postgresDDL and schemaPostgres,
// so match on the shape — a const whose value is a backtick-quoted literal —
// rather than on the name.
func extractDDLConst(src string) (string, error) {
	const marker = "const "
	idx := strings.Index(src, marker)
	if idx < 0 {
		return "", fmt.Errorf("no const declaration")
	}
	open := strings.Index(src[idx:], "`")
	if open < 0 {
		return "", fmt.Errorf("const is not a raw string literal")
	}
	open += idx + 1
	closeIdx := strings.Index(src[open:], "`")
	if closeIdx < 0 {
		return "", fmt.Errorf("unterminated raw string literal")
	}
	ddl := src[open : open+closeIdx]
	if !strings.Contains(strings.ToUpper(ddl), "CREATE TABLE") {
		return "", fmt.Errorf("extracted literal contains no CREATE TABLE — wrong const")
	}
	return ddl, nil
}
