package store

import (
	"go/scanner"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// claim_live_predicate_drift_test.go — every READ of style_node_claims that
// decides live behaviour must exclude retired rows.
//
// THE SYMPTOM WAS TWO QUERIES; THIS IS THE CLASS. A composer save retires
// rows routinely — every replace_all that drops a position does — so a read
// without the predicate answers with a claim the flow no longer has. Both
// misses had the same shape: written before retirement landed, or written
// against a sibling read that already had the filter, and neither had a test
// that would notice. ClaimForLinesidePayload resolved "where does THIS bin
// belong" off a retired row, and ListBackPositionNames captioned a position
// "back position" from a staging slot a save had dropped.
//
// live_reads_skip_retired_test.go proves the behaviour for the two
// process-wide reads. This proves the SHAPE for the whole tree, so the next
// read of this table cannot be written without the predicate or an entry
// here saying why it does not need one.
//
// The allowlist is by a distinctive fragment of the query rather than by
// file:line, because a line number goes stale on the next edit and the
// reason does not.
func TestClaimReads_CarryTheLivePredicate(t *testing.T) {
	t.Parallel()

	// Reads that are correct WITHOUT the predicate, each with the reason.
	// A by-id or by-(style,node) lookup is the write path resolving a
	// specific row it is about to touch, and retirement is not its question.
	allowed := map[string]string{
		"FROM style_node_claims WHERE id=?":                                      "GetClaim: by id, the caller already has the row",
		"SELECT id FROM style_node_claims WHERE style_id=? AND core_node_name=?": "the upsert's own existence check",
		"SELECT COALESCE(MAX(sequence), 0) FROM style_node_claims":               "next sequence: retired rows still hold their number",
		"SELECT below_reorder_since FROM style_node_claims WHERE id = ?":         "read-back of a stamp this call just wrote",
	}

	// Files whose every statement is outside the live-behaviour question.
	skipFiles := map[string]string{
		"migrations.go":              "migrations run over all rows, retired included",
		"migrations_style_claims.go": "the legacy table backfill",
		"claim_quarantine.go":        "quarantine moves rows BY swap_mode, deliberately including retired ones",
	}

	root := moduleRoot(t)
	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if why, skip := skipFiles[filepath.Base(path)]; skip {
			_ = why
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		for _, q := range rawStringLiterals(string(src)) {
			if !isClaimRead(q.text) {
				continue
			}
			if strings.Contains(q.text, "retired_at IS NULL") || strings.Contains(q.trailer, "liveClaims") {
				continue
			}
			if why := matchAllowed(allowed, q.text); why != "" {
				continue
			}
			offenders = append(offenders, rel+":"+lineOf(string(src), q.start)+"  "+oneLine(q.text))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(offenders) > 0 {
		t.Errorf("these reads of style_node_claims do not exclude retired rows.\n"+
			"Add `AND retired_at IS NULL` (or `+liveClaims`), or add the query to the allowlist\n"+
			"above with the reason it is correct without it:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// isClaimRead reports whether a SQL literal READS the claims table. A
// statement whose verb is a write is not a read even when it names the table
// in a FROM (INSERT ... SELECT).
func isClaimRead(sql string) bool {
	if !strings.Contains(sql, "FROM style_node_claims") && !strings.Contains(sql, "JOIN style_node_claims") {
		return false
	}
	head := strings.ToUpper(strings.TrimSpace(sql))
	for _, verb := range []string{"INSERT", "UPDATE", "DELETE", "CREATE", "ALTER", "DROP"} {
		if strings.HasPrefix(head, verb) {
			return false
		}
	}
	return true
}

func matchAllowed(allowed map[string]string, sql string) string {
	flat := strings.Join(strings.Fields(sql), " ")
	for frag, why := range allowed {
		if strings.Contains(flat, strings.Join(strings.Fields(frag), " ")) {
			return why
		}
	}
	return ""
}

type rawLiteral struct {
	text  string
	start int
	// trailer is what follows the literal's closing backtick, so a query
	// built as `... AND`+liveClaims is seen whole.
	trailer string
}

// rawStringLiterals is every backtick string in a Go file — and COMMENTS ARE
// NOT CODE.
//
// The scan used to walk the raw bytes for backticks, which is the same thing
// to a `SELECT ...` in a query and to a `SELECT ...` quoted inside a doc
// comment. This repo quotes SQL in comments constantly, and on 2026-09-13 one
// of them (domain/flow.go, recording the plant count that retired a field) was
// reported as a read of style_node_claims missing its live predicate. A drift
// test that fires on prose teaches people to stop writing prose.
//
// go/scanner is the parser Go itself uses, so a comment is a COMMENT token and
// never reaches this.
func rawStringLiterals(src string) []rawLiteral {
	var out []rawLiteral
	fset := token.NewFileSet()
	// The base is taken BEFORE AddFile: fset.Base() answers where the NEXT
	// file starts, so reading it afterwards offsets every position by the
	// length of this one.
	base := fset.Base()
	file := fset.AddFile("", base, len(src))
	var sc scanner.Scanner
	sc.Init(file, []byte(src), nil, 0)
	for {
		pos, tok, lit := sc.Scan()
		if tok == token.EOF {
			break
		}
		if tok != token.STRING || !strings.HasPrefix(lit, "`") {
			continue
		}
		start := int(pos) - base
		tail := start + len(lit)
		trailer := src[tail:]
		if len(trailer) > 60 {
			trailer = trailer[:60]
		}
		out = append(out, rawLiteral{text: strings.Trim(lit, "`"), start: start, trailer: trailer})
	}
	return out
}

func lineOf(src string, offset int) string {
	return strconv.Itoa(1 + strings.Count(src[:offset], "\n"))
}

func oneLine(s string) string {
	flat := strings.Join(strings.Fields(s), " ")
	if len(flat) > 110 {
		return flat[:110] + "…"
	}
	return flat
}

// moduleRoot walks up from the test's directory to the go.mod that owns it.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// TestRawStringLiterals_SkipsComments: the scanner reads code, not prose.
//
// This walked raw bytes for backticks once, which made a SELECT quoted inside
// a doc comment indistinguishable from a query — and it fired on the comment
// in domain/flow.go recording the plant counts that retired keep_staged. A
// drift test that fires on prose teaches people to stop writing prose.
// go/scanner knows a COMMENT token from a STRING one.
//
// The old byte scan was kept beside this so the assertion could show it
// finding two literals where the token scan finds one. That is a museum
// piece — an assertion about code nothing else calls. The property is this
// test's own subject: the token scan returns exactly the one literal that is
// code.
func TestRawStringLiterals_SkipsComments(t *testing.T) {
	t.Parallel()
	src := strings.Join([]string{
		"package p",
		"// The plant count that retired the field:",
		"// `SELECT COUNT(*) FROM style_node_claims WHERE keep_staged = 1`",
		"var q = `SELECT id FROM style_node_claims WHERE retired_at IS NULL`",
	}, "\n")

	got := rawStringLiterals(src)
	if len(got) != 1 {
		t.Fatalf("the token scan found %d literals, want 1: %+v", len(got), got)
	}
	if !strings.Contains(got[0].text, "retired_at IS NULL") {
		t.Errorf("the one literal is %q; the comment above it is not code", got[0].text)
	}
}
