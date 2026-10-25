package plantclaims

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// legColumns are the four claim legs the S0 demand loop carries into the
// mirror: where a node's inbound material comes from, where its outbound goes,
// and the paired positions of a two-robot cell. Spelled as the COLUMN names,
// because what this file is about is SQL.
var legColumns = []string{
	"inbound_source",
	"outbound_destination",
	"paired_core_node",
	"second_paired_core_node",
}

// TestLegColumnsAreNotReadByTheSourceabilityPath pins the boundary between the
// two things style_claims is now for.
//
// The mirror answers ONE question today — can this (process, style) be
// sourced — and it answers it from payload_code, allowed_payload_codes and
// core_node_name. The legs are for a SECOND question, asked by the loop
// compiler: which nodes feed which. They ride in the same table because they
// arrive in the same message and are replaced wholesale with it, not because
// the sourceability verdict has anything to do with them.
//
// So a leg must not reach the recompute, and the reason is not tidiness. The
// recompute reads this table's CONTENT through loadStylesAndClaims, which
// decides the verdict and from whose output the monitor builds its own payload →
// style dirty index. A leg projected into it would make a changeover-position
// edit — which changes no payload and no stock — move a verdict or mark styles
// dirty, and a sourceability verdict that moves when nothing about sourcing
// moved is a false alarm on an operator screen. The guard also read a dirty
// index in plantclaims; it had no production caller and was removed in Stage A.
//
// A SOURCE TEST because the property is about the SELECT list, and a
// behavioural fixture can only demonstrate the cases somebody thought to
// write. What must not drift is which columns the recompute projects, and that
// is checkable directly.
func TestLegColumnsAreNotReadByTheSourceabilityPath(t *testing.T) {
	t.Parallel()

	// The sourceability package never writes style_claims — it only reads the
	// mirror — so there its whole files are the right grain.
	//
	// RULED IN D2.1 (DECISIONS D17): the recompute's claim load also carries
	// inbound_source, because the forecaster on the same two-minute pass plans a
	// cell's supply from it and a read of its own would cost a statement the
	// brief did not name. What this test protects is the verdict and the dirty
	// index, and neither reads it: the verdict's file (sourceability.go) must not
	// name the field, checked below. Every other leg stays out of the load.
	for what, c := range map[string]struct {
		path  string
		allow string
	}{
		"the recompute's claim load (sourceability.loadStylesAndClaims)": {filepath.Join("..", "sourceability", "read.go"), "inbound_source"},
		"the sourcing page's per-process read":                           {filepath.Join("..", "sourceability", "read_page.go"), ""},
	} {
		b, err := os.ReadFile(filepath.Clean(c.path))
		if err != nil {
			t.Fatalf("read %s: %v", c.path, err)
		}
		assertNoLegColumnsBut(t, what+" ("+c.path+")", string(b), c.allow)
	}
	verdict, err := os.ReadFile(filepath.Clean(filepath.Join("..", "sourceability", "sourceability.go")))
	if err != nil {
		t.Fatalf("read the verdict: %v", err)
	}
	if strings.Contains(string(verdict), "InboundSource") {
		t.Error("the sourceability verdict (sourceability.go) names InboundSource: a leg now moves a verdict")
	}
}

func assertNoLegColumnsBut(t *testing.T, what, src, allow string) {
	t.Helper()
	for _, col := range legColumns {
		if col != allow && strings.Contains(src, col) {
			t.Errorf("%s now names style_claims.%s: the legs are the loop compiler's input, not the "+
				"sourceability verdict's. A ruling changes this test.", what, col)
		}
	}
}

// sanctionedLegReaders are the files allowed to read a leg column of
// style_claims, each with the reason it may. A new reader is a decision, not an
// accident: add it here with its reason, or move the read behind one of these.
var sanctionedLegReaders = map[string]string{
	"store/plantclaims/plantclaims.go": "the writer (ReplaceProcess), and the addressing read: a station binds at its cell's paired positions too (Lane H, H.3)",
	"store/return_sources.go":          "the cancel-return policy: where a line's bins come from and its empties go",
	"store/containment.go":             "the containment divert scopes itself to the claim's own outbound flow",
	"store/migrations.go":              "the schema",
	"store/bins/bins.go":               "the empty scan leaves a cell's own positions alone, a press-index cell's paired positions included",
	"store/bins/tote_want.go":          "the height an empty is wanted at: 0 at the press of a cell fed from its paired position (FedClaimSQL's shape test), read with the set-down node",
	"store/bins/fed_claim.go":          "the fed press's shape (FedClaimSQL) and where it draws from for a vacant pickup at its outside or second paired position (FedInboundSource)",
	"store/bins/tote_lift.go":          "the lift's tote hook: is the node a fed cell's outside position (FedClaimSQL), and which press is its front",
	"store/sourceability/read.go":      "the two-minute pass's claim load carries inbound_source for the forecaster (D2.1); the verdict does not read it",
	"cmd/soakstat/main.go":             "the sim reader's vacated-line holds: a pair parked while its fed press's paired or second paired position holds no bin, the shape composed through FedClaimSQL; read-only instrumentation",
}

// TestLegColumnsHaveOnlySanctionedReaders walks shingo-core for any file that
// names style_claims beside a leg column. The sourceability boundary above is
// one rule; this is the census it sits in, so a third reader cannot arrive
// without somebody writing down why.
func TestLegColumnsHaveOnlySanctionedReaders(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == "testdata" || info.Name() == "schema" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		src := string(b)
		if !strings.Contains(src, "style_claims") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		for _, col := range legColumns {
			if strings.Contains(src, col) {
				if _, ok := sanctionedLegReaders[rel]; !ok {
					t.Errorf("%s names style_claims and the leg %s, and is not a sanctioned leg reader.\n"+
						"Add it to sanctionedLegReaders with the reason it may read the legs, "+
						"or read them through one of the files already there.", rel, col)
				}
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}
