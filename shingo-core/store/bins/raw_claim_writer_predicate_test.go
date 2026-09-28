package bins_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// raw_claim_writer_predicate_test.go — the WHERE-clause pins on the raw
// `claimed_by` status writers.
//
// WHY THESE PINS EXIST. The coupled release helpers (ReleaseClaimForBin,
// TerminalizeOrder, DemoteHoldsAfterFleetRefusal) clear bins/nodes.claimed_by
// WITH the coupled reservation release, and the forbidigo Unclaim guard stops
// a bare bins.Unclaim call from coming back. But the raw SQL sites below
// write `claimed_by=NULL` as string literals inside larger statements —
// forbidigo cannot see them (it matches identifier selectors, not SQL text),
// so nothing mechanical guards their WHERE shape. A predicate silently widened
// from "this order's claims" to "all claims" (a dropped WHERE, a changed key)
// would brick bins via orphaned reservations and no test would name the site.
//
// These are pins of CURRENT behaviour, not change: each entry names the exact
// scoping the statement's WHERE must carry, so a widening fails the test and
// a narrowing fails it too. No database needed — the claim is about source
// text, the same contract one_sourcing_predicate_test.go pins.
//
// VERIFIED RED BY: widening one WHERE in a scratch edit — dropping
// `AND claimed_by=$2` from ReleaseClaimForBin's bins statement failed its row.

// rawClaimWriters are the statements that hand-write `claimed_by=NULL`
// (or `claimed_by = NULL`) rather than going through a guarded primitive.
// Each must carry the scoping in `mustContain` — the WHERE key that keeps the
// release scoped to one order (or one bin+owner pair).
var rawClaimWriters = []struct {
	file, fn     string
	mustContain  string
	why          string
	noClaimClear bool
}{
	// ── Core: the terminal/park chokepoint and its two callers ──────────
	{
		file: "../orders.go", fn: "func (db *DB) TerminalizeOrderWithReason(",
		mustContain:  "",
		why:          "TerminalizeOrderWithReason itself does not hand-write a claim clear — releaseOrderHoldingsTx does, and the pin sits on that function's row below. This row exists to fail loudly if a claimed_by=NULL statement ever appears here instead (a second spelling beside the chokepoint).",
		noClaimClear: true,
	},
	{
		file: "../orders.go", fn: "func releaseOrderHoldingsTx(",
		mustContain: "WHERE claimed_by=$1",
		why:         "The terminal chokepoint's claim release. Order-scoped both halves (bins and nodes): a dropped WHERE clears EVERY order's claims in one terminalize.",
	},
	{
		file: "../orders.go", fn: "func (db *DB) ReleaseClaimForBin(",
		mustContain: "WHERE id=$1 AND claimed_by=$2",
		why:         "The one-bin coupled release. Bin-AND-owner scoped: a dropped owner arm would clear another order's claim off a bin this order never held.",
	},
	{
		file: "../orders.go", fn: "func (db *DB) DemoteHoldsAfterFleetRefusal(",
		mustContain: "WHERE claimed_by=$1",
		why:         "The fleet-refusal demote. Order-scoped: this releases only the refusing order's armor, not the corridor's.",
	},
	// ── Core: recovery ──────────────────────────────────────────────────
	{
		file: "../recovery/recovery.go", fn: "func ReleaseTerminalBinClaim(",
		mustContain: "WHERE id=$1",
		why:         "Recovery's guarded release. Bin-scoped by id, and only after the guard above confirmed the claiming order is terminal — the terminal-check guard is the load-bearing part and lives just above the statement; this pin keeps the WHERE from ever widening past the one bin.",
	},
	// ── Core: the placement primitive ───────────────────────────────────
	{
		file: "../internal/helpers/place_bin.go", fn: "func PlaceBinTx(",
		mustContain: "WHERE id=$1",
		why:         "The arrival placement clears the bin's claim (handoff) and the destination slot's claim, both node/bin-scoped by id; the bin arm additionally carries the compound-sibling exemption spelled beside it.",
	},
	{
		file: "../nodes.go", fn: "func (db *DB) ReleaseSlotClaim(",
		mustContain: "WHERE id=$1 AND claimed_by=$2",
		why:         "The slot twin of ReleaseClaimForBin. Node-AND-owner scoped, with the coupled reservation release in the same tx.",
	},
	// ── Core: slot primitives ───────────────────────────────────────────
	{
		file: "../nodes/nodes.go", fn: "func UnclaimSlot(",
		mustContain: "WHERE id=$1",
		why:         "Single-slot unclaim, node-scoped by id.",
	},
	{
		file: "../nodes/nodes.go", fn: "func UnclaimOrderSlots(",
		mustContain: "WHERE claimed_by=$1",
		why:         "Order-scoped slot unclaim: a dropped WHERE clears every slot claim in the plant.",
	},
}

// TestRawClaimWritersKeepTheirScope pins each writer's WHERE shape.
//
// The read is the same readBody one_sourcing_predicate_test.go uses: cut the
// file at the function declaration, take everything to the next top-level
// `func `, and look at that text. The scoping key must appear in the body;
// for the noClaimClear rows, NO claimed_by=NULL statement may appear at all.
func TestRawClaimWritersKeepTheirScope(t *testing.T) {
	t.Parallel()
	for _, w := range rawClaimWriters {
		body := readBody(t, resolveRowFile(t, w.file), w.fn)
		clears := rawClaimClearRE.MatchString(stripGoComments(body))
		if w.noClaimClear {
			if clears {
				t.Errorf("%s (%s) hand-writes a claimed_by clear.\n\n%s",
					w.fn, w.file, w.why)
			}
			continue
		}
		if !clears {
			t.Errorf("%s (%s) no longer writes a raw claimed_by clear — if the writer moved to a "+
				"guarded primitive, repoint this guard rather than deleting it", w.fn, w.file)
		}
		if !strings.Contains(body, w.mustContain) {
			t.Errorf("%s (%s) lost its scoping: WHERE must contain %q.\n\n%s",
				w.fn, w.file, w.mustContain, w.why)
		}
	}
}

// rawClaimClearRE is the brief's `claimed_by\s*=\s*NULL`. Case-insensitive
// because SQL is: `claimed_by = null` clears exactly as well.
var rawClaimClearRE = regexp.MustCompile(`(?i)\bclaimed_by\s*=\s*NULL\b`)

// rawClaimCensusKnown are files that write claimed_by=NULL outside the rows
// above on purpose, each with its reason. Paths are repo-relative. Legacy
// migrations are exempt by file rule (isLegacyMigration), not listed here.
var rawClaimCensusKnown = map[string]string{
	"shingo-core/store/reconciliation/reconciliation.go": "repair sweeps re-derive scoping from orphan-state joins; docker-pinned there",
}

// TestRawClaimWritersAreAllAccountedFor walks every non-test Go file in the
// five modules — not only Core's store/: a claim clear written from a service,
// an engine or the Edge is the same unwatched second spelling — and fails on a
// claimed_by=NULL in a file with no row and no reasoned entry. A known entry
// that no longer matches fails too: a dead allowlist row reads as coverage.
func TestRawClaimWritersAreAllAccountedFor(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	pinned := map[string]bool{}
	for _, w := range rawClaimWriters {
		pinned[resolveRowFile(t, w.file)] = true
	}
	used := map[string]bool{}
	for _, f := range repoGoFiles(t, root) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		// COMMENT-ONLY MENTIONS DO NOT COUNT. A file whose only occurrence
		// is inside a // comment is narrating a writer, not being one, so
		// strip comments before matching. (order_bins.go:74 cites the fixed
		// pre-445f79eb shape in prose and would otherwise need a fake row.)
		if !rawClaimClearRE.MatchString(stripGoComments(string(b))) {
			continue
		}
		rel := repoRel(t, root, f)
		if pinned[f] || isLegacyMigration(rel) {
			continue
		}
		if _, ok := rawClaimCensusKnown[rel]; ok {
			used[rel] = true
			continue
		}
		t.Errorf("%s writes claimed_by=NULL and has no row in rawClaimWriters — a raw "+
			"claim writer appearing unwatched is how the guarded primitives get a second "+
			"spelling. Route it through the coupled release, or add a row", rel)
	}
	for rel, why := range rawClaimCensusKnown {
		if !used[rel] {
			t.Errorf("rawClaimCensusKnown names %s (%s), but it no longer writes claimed_by=NULL — "+
				"delete the entry", rel, why)
		}
	}
}

// stripGoComments removes // line comments and /* */ block comments from Go
// source. It skips over string, raw-string and rune literals so a "/*" inside
// one (a route glob such as "/static/*", a template glob) cannot swallow the
// code after it — the census walks the whole repo, where such literals exist,
// and an over-eager strip there would hide a writer rather than invent one.
func stripGoComments(src string) string {
	var out []byte
	for i := 0; i < len(src); {
		switch c := src[i]; {
		case strings.HasPrefix(src[i:], "//"):
			j := strings.IndexByte(src[i:], '\n')
			if j < 0 {
				i = len(src)
			} else {
				i += j
			}
		case strings.HasPrefix(src[i:], "/*"):
			j := strings.Index(src[i+2:], "*/")
			if j < 0 {
				i = len(src)
			} else {
				i += j + 4
			}
		case c == '`':
			j := strings.IndexByte(src[i+1:], '`')
			end := len(src)
			if j >= 0 {
				end = i + 1 + j + 1
			}
			out = append(out, src[i:end]...)
			i = end
		case c == '"' || c == '\'':
			j := i + 1
			for j < len(src) && src[j] != c && src[j] != '\n' {
				if src[j] == '\\' {
					j++
				}
				j++
			}
			if j < len(src) && src[j] == c {
				j++
			}
			if j > len(src) {
				j = len(src)
			}
			out = append(out, src[i:j]...)
			i = j
		default:
			out = append(out, c)
			i++
		}
	}
	return string(out)
}
