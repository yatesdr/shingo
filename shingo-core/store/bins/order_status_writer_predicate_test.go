package bins_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// order_status_writer_predicate_test.go — the WHERE-shape pins on the raw
// `UPDATE orders SET status` writers.
//
// WHY THESE PINS EXIST. Movement through the order state machine is supposed
// to go through transition() (dispatch/lifecycle.go), whose CAS writes
// (UpdateStatusFrom) are the recorded answer to a stale-snapshot resurrect of
// a cancelled order. But a handful of store-level functions hand-write
// `UPDATE orders SET status` as raw SQL, and forbidigo cannot see them — it
// matches identifier selectors like db.UpdateOrderStatus, not SQL text
// inside string literals (docs/order-state-machine/transitions.md, "What
// bypasses transition()"). Nothing mechanical guarded these sites' WHERE
// shape; a silently dropped status guard arm would let a stale writer
// overwrite a terminal status and nothing would name the site.
//
// These are pins of CURRENT behaviour, not change: each row names the exact
// scoping the statement's WHERE must carry, so a widening (dropped `AND
// status=$N`) or a narrowing fails the test and names the function. No
// database needed — the claim is about source text, the same contract
// raw_claim_writer_predicate_test.go pins for claimed_by.
//
// VERIFIED RED BY: dropping `AND status=$4` from UpdateStatusFrom's
// statement in a scratch edit — its row fails.

// orderStatusWriters are the raw `UPDATE orders SET status` statements.
// mustStatusGuard is the status-arm the WHERE must carry.
var orderStatusWriters = []struct {
	file, fn, mustStatusGuard, why string
}{
	// ── Core: order store ──────────────────────────────────────────────
	{
		file: "../orders.go", fn: "func (db *DB) TerminalizeOrderWithReason(",
		mustStatusGuard: "AND status <> ALL($4)",
		why:             "The terminal chokepoint's raw status write. The `AND status <> ALL($4)` arm refuses to terminalize a row already in a terminal status — a dropped arm double-terminalizes and writes a second terminal history row. (TerminalizeOrder delegates here.)",
	},
	{
		file: "../orders/orders.go", fn: "func UpdateStatus(",
		mustStatusGuard: "WHERE id=$3",
		why:             "The id-scoped write. Wait-ends bookkeeping (waitEndsSQL) rides along; the id WHERE is the whole guard — a dropped WHERE would re-stamp every order.",
	},
	{
		file: "../orders/orders.go", fn: "func UpdateStatusFromWithReason(",
		mustStatusGuard: "AND status=$4",
		why:             "The CAS write transition() rides on. The AND status=$4 arm is the compare-and-swap itself; without it every transition is a blind overwrite and the resurrect incident (stale scanner wrote queued→sourcing over a cancel) is re-opened.",
	},
	// ── Edge: order store ──────────────────────────────────────────────
	{
		file: "@edge/store/orders/orders.go", fn: "func UpdateStatus(",
		mustStatusGuard: "WHERE id=?",
		why:             "Edge's id-scoped status write. Edge statuses mirror Core's machine for the operator board; a dropped WHERE re-stamps the whole board.",
	},
}

// TestOrderStatusWritersKeepTheirScope pins each writer's WHERE shape: the
// function body must contain the raw status UPDATE and the named guard.
func TestOrderStatusWritersKeepTheirScope(t *testing.T) {
	t.Parallel()
	for _, w := range orderStatusWriters {
		body := readBody(t, resolveRowFile(t, w.file), w.fn)
		if !orderStatusWriteRE.MatchString(stripGoComments(body)) {
			t.Errorf("%s (%s) no longer hand-writes a status UPDATE — if the writer moved to a "+
				"guarded primitive, repoint this guard rather than deleting it", w.fn, w.file)
		}
		if !strings.Contains(body, w.mustStatusGuard) {
			t.Errorf("%s (%s) lost its scoping: WHERE must contain %q.\n\n%s",
				w.fn, w.file, w.mustStatusGuard, w.why)
		}
	}
}

// orderStatusWriteRE is the brief's `UPDATE orders SET status`, tolerant of
// SQL's whitespace (a statement split across lines of a raw string is the
// common spelling here) and case.
var orderStatusWriteRE = regexp.MustCompile(`(?i)\bUPDATE\s+orders\s+SET\s+status\b`)

// orderStatusCensusExempt are the non-test files allowed to write a status
// outside the rows above, each with its reason. Legacy migrations are exempt by
// file rule (isLegacyMigration), not listed here.
var orderStatusCensusExempt = map[string]string{
	"shingo-core/internal/testdb/testdb.go": "SeedOrderStatus is a test fixture: internal/testdb is imported only by _test.go files and the test harness, and it seeds a status deliberately so a test can start mid-machine",
}

// TestOrderStatusWritersAreAllAccountedFor walks every non-test Go file in
// the five modules and fails when a raw status UPDATE appears in a file with
// no row above — the same census raw_claim_writer_predicate_test.go runs for
// claimed_by. Legacy migrations are exempt by file: one-shot data repairs die
// with their migration. An exemption that no longer matches fails too.
func TestOrderStatusWritersAreAllAccountedFor(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	pinned := map[string]bool{}
	for _, w := range orderStatusWriters {
		pinned[resolveRowFile(t, w.file)] = true
	}
	used := map[string]bool{}
	for _, f := range repoGoFiles(t, root) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		// COMMENT-ONLY MENTIONS DO NOT COUNT (see the claimed_by twin).
		if !orderStatusWriteRE.MatchString(stripGoComments(string(b))) {
			continue
		}
		rel := repoRel(t, root, f)
		if pinned[f] || isLegacyMigration(rel) {
			continue
		}
		if _, ok := orderStatusCensusExempt[rel]; ok {
			used[rel] = true
			continue
		}
		t.Errorf("%s writes UPDATE orders SET status and has no row in orderStatusWriters — "+
			"an unwatched raw status writer is how the state machine grows a second spelling. "+
			"Route the write through transition(), or add a row", rel)
	}
	for rel, why := range orderStatusCensusExempt {
		if !used[rel] {
			t.Errorf("orderStatusCensusExempt names %s (%s), but it no longer writes a status — "+
				"a dead exemption reads as coverage; delete the entry", rel, why)
		}
	}
}

// repoRoot is the directory holding go.work and the five modules. Found by
// walking up from the test's working directory rather than by a relative
// literal, so the guard reads the same tree in the main checkout, any
// worktree (whatever its folder is called) and CI. It fails rather than
// skips when the Edge module is absent: a census that silently walks one
// module passes on half the tree.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolve cwd: %v", err)
	}
	for {
		if fileExists(filepath.Join(dir, "go.work")) &&
			fileExists(filepath.Join(dir, "shingo-core", "go.mod")) &&
			fileExists(filepath.Join(dir, "shingo-edge", "go.mod")) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no ancestor of the test directory holds go.work + shingo-core + shingo-edge — " +
				"these guards census both modules and cannot run on half a checkout")
		}
		dir = parent
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// resolveRowFile maps a row's file to an absolute path. "@edge/..." is rooted
// at the repo's shingo-edge module; anything else is relative to this package.
func resolveRowFile(t *testing.T, file string) string {
	t.Helper()
	if rest, ok := strings.CutPrefix(file, "@edge/"); ok {
		return filepath.Join(repoRoot(t), "shingo-edge", filepath.FromSlash(rest))
	}
	abs, err := filepath.Abs(filepath.FromSlash(file))
	if err != nil {
		t.Fatalf("resolve %s: %v", file, err)
	}
	return abs
}

// censusModules are the five go.work modules; both censuses walk all of them.
var censusModules = []string{"protocol", "shared", "shingo-core", "shingo-edge", "integration"}

// repoGoFiles is every non-test .go file in the five modules (testdata and
// node_modules skipped). Fails when the walk comes back implausibly small.
func repoGoFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	for _, mod := range censusModules {
		err := filepath.WalkDir(filepath.Join(root, mod), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" || d.Name() == "node_modules" {
					return fs.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
				out = append(out, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", mod, err)
		}
	}
	if len(out) < 500 {
		t.Fatalf("walked only %d non-test .go files under %s — a census that walks nothing passes on nothing", len(out), root)
	}
	return out
}

// repoRel is f relative to the repo root, slash-separated.
func repoRel(t *testing.T, root, f string) string {
	t.Helper()
	rel, err := filepath.Rel(root, f)
	if err != nil {
		t.Fatalf("rel %s: %v", f, err)
	}
	return filepath.ToSlash(rel)
}

// isLegacyMigration is the by-file exemption both censuses share: a
// migrations.go chain file or anything under a migrations/ directory.
func isLegacyMigration(rel string) bool {
	return strings.HasSuffix(rel, "/migrations.go") || strings.Contains(rel, "/migrations/")
}
