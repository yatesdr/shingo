package engine

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

var updateSteadyPageFixture = flag.Bool("update-steady-page-fixture", false,
	"rewrite www/testdata/process-setup-harness/steady.json from the builders")

// steadyPageEntry is one cell the Processes page's harness may preview, the
// steady state the builders give it, and the validator's findings on it.
type steadyPageEntry struct {
	StyleID  int64           `json:"style_id"`
	Cell     domain.FlowCell `json:"cell"`
	RefillBy string          `json:"refill_by"`
	Steady   SteadyCell      `json:"steady"`
	// Findings is what the preview's validator says of the cell alone
	// (validateFlowInputs, over a context it could not look in, so no
	// membership or key-route check), so a page pin shows the sentence a real
	// preview gives. ONLY ON A DRAFT WRITTEN AS A SWAP NOT COMPLETE: the stored
	// claims' own findings are not served, because Style B (every setting set)
	// is refused by the validator for its tool change (POS_A11 marked, not one
	// of the claim's nodes; no inbound staging), and answering that would block
	// every Style B pin's save.
	Findings []domain.NodeFinding `json:"findings,omitempty"`
}

// TestSteady_PageHarnessFixture writes the steady state the Processes page's
// harness serves for flow/preview?steps=1 (process-setup-harness/pins.js), so
// the page's work-list pins draw the builders' own legs and the harness holds
// no step of its own.
//
// Each of the harness fixture's stored claims is drafted as the page sends it
// unedited (domain.Collapse, then the preview's own draftFlow path: Expand and
// MaterializeClaim over the claim as prior) and, on an index press, once more
// per Next carrier (the cell's Advanced, as the page sends it once Next
// carrier is set); a 2-Robot Swap once more with its destination cleared, the
// builder's refusal, with the validator's findings on it. The harness serves
// an entry only for a cell that matches it field for field, its findings with
// it; any other draft gets no steady state and no finding.
func TestSteady_PageHarnessFixture(t *testing.T) {
	dir := filepath.Join("..", "www", "testdata", "process-setup-harness")
	raw, err := os.ReadFile(filepath.Join(dir, "fixture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Composer struct {
			Styles []struct {
				ID     int64                 `json:"id"`
				Claims []processes.NodeClaim `json:"claims"`
			} `json:"styles"`
		} `json:"composer"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	var out []steadyPageEntry
	add := func(styleID int64, prior processes.NodeClaim, cell domain.FlowCell, r protocol.RefillBy, incomplete bool) {
		if r != "" {
			a := domain.AdvancedOf(&prior)
			a.RefillBy = r
			cell.Advanced = a
		}
		in := domain.Expand(cell, &prior, domain.ClaimSourceHMI, "")
		in.StyleID = styleID
		claim := domain.MaterializeClaim(in, &prior)
		cell.Advanced = nil
		e := steadyPageEntry{StyleID: styleID, Cell: cell, RefillBy: string(r), Steady: steadyCell(&claim)}
		if incomplete {
			e.Findings = validateFlowInputs(domain.ClaimContextSet{}, []domain.NodeClaimInput{in})
		}
		out = append(out, e)
	}
	for _, st := range fx.Composer.Styles {
		for i := range st.Claims {
			prior := st.Claims[i]
			add(st.ID, prior, domain.Collapse(prior), "", false)
			if prior.IsPressIndex() {
				for _, r := range []protocol.RefillBy{protocol.RefillByClearingRobot, protocol.RefillByIndexRobot, protocol.RefillByOwnRobot} {
					add(st.ID, prior, domain.Collapse(prior), r, false)
				}
			}
			// A swap not complete: a 2-Robot Swap drafted with its
			// destination cleared, which BuildSwapDispatch refuses.
			if prior.SwapMode == protocol.SwapModeTwoRobot {
				cell := domain.Collapse(prior)
				cell.OutboundDestination = ""
				add(st.ID, prior, cell, "", true)
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("the harness fixture has no stored claims")
	}
	got, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join(dir, "steady.json")
	if *updateSteadyPageFixture {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no %s (%v); regenerate with -update-steady-page-fixture", path, err)
	}
	if !bytes.Equal(bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")), got) {
		t.Errorf("%s is not what the builders give the harness fixture's claims; regenerate with -update-steady-page-fixture and read the diff", path)
	}
}
