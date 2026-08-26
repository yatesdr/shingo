//go:build docker

package store_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// bin_placement_totality_test.go — where a bin IS has one writer.
//
// ── WHY THIS IS A CENSUS AND NOT A RULE IN A DOC ──────────────────────────
//
// bins.node_id already had exactly one home. What it did not have was one
// WRITER: three code paths put a bin down — the single-bin arrival, the
// multi-bin settle, and the operator's completion repair — each spelling the
// same five steps in its own words, and the words drifted. Every defect this
// batch opened with lived in that drift:
//
//   - 445f79eb scoped the arrival's unclaim to the placing order and coupled
//     the bin reservation to it. The other two writers did not get the fix, so
//     a dig leg setting a blocker down still cleared whoever's claim it found —
//     order 1 failed on cargo_ledger_mismatch and took its swap partner with it,
//     twice in a 17-minute window.
//   - the repair path had no ReleaseByBin AT ALL, so a repaired bin stayed
//     reserved until its owning order terminalized.
//   - the repair path had no ghost eviction either, until it was added by hand.
//
// helpers.EvictStaleGhostBinsTx is the precedent: extracted for exactly this
// reason ("so the paths cannot drift and no caller can forget the synthetic
// exemption"), and it held. helpers.PlaceBinTx is the rest of the placement.
//
// A census rather than a comment, because a comment is what the three writers
// already had. This is the writer_totality_test pattern from store/orders: read
// the shipped source as text, and make every exception a line somebody had to
// type with a reason attached.

// placementWriterNeedle matches an UPDATE that writes bins.node_id. Deliberately
// loose — it catches `SET node_id=`, and also the multi-column form
// `SET bin_type_id=..., node_id=...` that a full-row rewrite uses, which is the
// one an exact-prefix match would miss. bins.Update is precisely that shape and
// is precisely the site the round flagged as a lost-update window.
const placementWriterNeedle = "node_id=$"

// placementWritersAllowed lists every file:function permitted to write
// bins.node_id outside the primitive, each with the reason it is not a
// placement.
//
// THE LIST IS THE POINT. A writer missing from the primitive is not
// automatically a bug — several of these genuinely move a bin without it being
// an arrival — but a writer missing from the primitive AND from this list is a
// path somebody added without deciding, and that is exactly how three arrival
// writers came to disagree about the unclaim.
//
// The cost of adding an entry is one line and one sentence. The cost of the
// alternative was measured on the rig in stranded bins.
var placementWritersAllowed = map[string]string{
	// The primitive itself.
	"internal/helpers/place_bin.go": "THE placement. Everything below is a writer that is not an arrival.",

	// The indirect writer the round named as the one that must not escape the
	// census: eviction moves the GHOST, not the arriving bin, and it is called
	// BY the primitive.
	"internal/helpers/helpers.go": "stale-ghost eviction moves the displaced bin to _TRANSIT; it is called by PlaceBinTx, not a peer of it",

	// bins-package primitives. These are not arrivals: nothing has been
	// delivered, no claim is ending, no slot is being freed.
	"bins/bins.go": "the bins aggregate's own movers — Retire (node_id=NULL), MoveAndClearStaging / MoveOffTransit (operator manual move, the second clearing the anomaly a bin coming off _TRANSIT or a deck carried), MoveToTransit (vendor pickup), RecoverToNode (operator anomaly recovery), Update (admin full-row write). None is a delivery; see the note in this test about Update.",
}

// TestPlacement_HasOneWriter is the census.
//
// It scans for `UPDATE bins` STATEMENTS rather than for lines, because SQL in
// this tree wraps: the line carrying `node_id=$1` often does not carry the
// table name, and the multi-column full-row form puts them several columns
// apart. Matching lines was the first attempt and it found nothing at all —
// including the primitive, which is what the sawPrimitive assertion below
// exists to catch. A census that cannot find its own subject asserts nothing.
//
// MUTATION (verified): re-inline the placement in store/order_bins.go — an
// `UPDATE bins SET node_id=$1 ...` in the multi-bin loop — and this names the
// file. Delete the primitive's own allowlist entry and it fires for
// place_bin.go.
func TestPlacement_HasOneWriter(t *testing.T) {
	t.Parallel()

	var offenders []string
	scanned := 0
	sawPrimitive := false

	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel := filepath.ToSlash(path)
		// The DDL and the snapshot DECLARE the column; they do not write it.
		if strings.Contains(rel, "/schema/") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		scanned++
		for _, stmt := range binsUpdateStatements(string(b)) {
			if !strings.Contains(stmt.body, placementWriterNeedle) {
				continue
			}
			key := allowKeyFor(rel)
			if _, ok := placementWritersAllowed[key]; ok {
				if key == "internal/helpers/place_bin.go" {
					sawPrimitive = true
				}
				continue
			}
			offenders = append(offenders, rel+":"+itoaPlacement(stmt.line))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk store tree: %v", err)
	}

	// A census that scanned nothing must not read as a census that found
	// nothing — the rule every instrument in this batch is built on.
	if scanned == 0 {
		t.Fatal("the census scanned ZERO files, so its silence means nothing")
	}
	// And it must find the primitive, or the needle stopped matching and every
	// other assertion here is vacuous.
	if !sawPrimitive {
		t.Fatal("the census did not find the placement primitive's own node_id write. Either it " +
			"moved — update placementWritersAllowed in the same commit — or the needle no longer " +
			"matches it, which makes this whole test assert nothing.")
	}

	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("bins.node_id is written at %d site(s) outside the primitive and outside the "+
			"allowlist: %s\n\n"+
			"Putting a bin down is SIX things in one transaction — ghost eviction, the node_id "+
			"write, the owner-scoped unclaim, the coupled bin reservation, the destination slot's "+
			"claim and reservation, and the staging state — and splitting any of them from the "+
			"others is what produced every defect this batch opened with. Use helpers.PlaceBinTx "+
			"(or db.PlaceBinTx from the service layer).\n\n"+
			"If this write is genuinely NOT a placement — a retire, a manual move, a transit "+
			"hop — add it to placementWritersAllowed with the reason.",
			len(offenders), strings.Join(offenders, ", "))
	}
}

// binsUpdate is one `UPDATE bins` statement: the source line it starts on, and
// enough of the text after it to cover its SET list.
type binsUpdate struct {
	line int
	body string
}

// binsUpdateStatements finds every `UPDATE bins` in a file and returns the text
// that follows it, up to the end of the statement.
//
// The terminator is the closing backtick of the raw string, or 600 characters,
// whichever comes first — long enough for the widest SET list in this tree
// (bins.Update's five columns) and short enough that it cannot run into the
// next statement.
func binsUpdateStatements(text string) []binsUpdate {
	var out []binsUpdate
	upper := strings.ToUpper(text)
	for i := 0; ; {
		j := strings.Index(upper[i:], "UPDATE BINS")
		if j < 0 {
			return out
		}
		at := i + j
		end := at + 600
		if end > len(text) {
			end = len(text)
		}
		if tick := strings.Index(text[at:end], "`"); tick >= 0 {
			end = at + tick
		}
		out = append(out, binsUpdate{
			line: strings.Count(text[:at], "\n") + 1,
			body: text[at:end],
		})
		i = at + len("UPDATE BINS")
	}
}

// allowKeyFor maps a path onto its allowlist key: the package-relative file for
// helpers (whose two files have different reasons) and the package directory
// otherwise.
func allowKeyFor(rel string) string {
	rel = strings.TrimPrefix(rel, "./")
	for k := range placementWritersAllowed {
		if strings.HasSuffix(rel, k) {
			return k
		}
	}
	return rel
}

func itoaPlacement(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
