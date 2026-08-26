package www

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A RESPONSE BODY NOBODY READS IS STILL A CONTRACT.
//
// Seven operator-action endpoints used to serialise the whole 37-field
// domain.Order they had just created. Nothing read any of it: the station is
// event-driven — the response carries an HX-Trigger and the client re-renders
// from the orders list — and every caller either awaited the promise and threw
// it away or went through postAction, which reads the body only on failure.
//
// Unread is not free. Every field in a shipped body is something a future
// reader can come to depend on and something this service then has to keep
// shaped that way; the round-3 audit found `cycle_mode` had already become the
// bad version of that — unread, and ambiguous for the one question anyone
// would have used it for.
//
// So these seven answer {"status":"ok"} plus the trigger. This test is what
// stops the next hand reaching for the order again, because nothing else can
// see the difference: putting it back compiles, passes every other test, and
// silently re-establishes the contract.
//
// ── WHAT IS DELIBERATELY NOT LISTED ───────────────────────────────────────
//
// /request and /finalize return NodeOrderResult and it is READ — round 2 gave
// prime_orders, order_a and order_b real consumers on the station. The
// changeover /start and /preview bodies are read by changeover.js. None of
// those are acknowledgements and none belong here.
func TestOperatorActionsAnswerWithAnAcknowledgement(t *testing.T) {
	// handler name -> the file it lives in.
	handlers := map[string]string{
		"apiReleaseNodeEmpty":                "handlers_operator_actions.go",
		"apiReleaseNodePartial":              "handlers_operator_actions.go",
		"apiRequestEmptyBin":                 "handlers_operator_bins.go",
		"apiRequestFullBin":                  "handlers_operator_bins.go",
		"apiStageNodeChangeoverMaterial":     "handlers_operator_changeover.go",
		"apiEvacuateNode":                    "handlers_operator_changeover.go",
		"apiDeliverNewMaterialForChangeover": "handlers_operator_changeover.go",
	}

	bodies := map[string]string{}
	for _, file := range []string{
		"handlers_operator_actions.go", "handlers_operator_bins.go", "handlers_operator_changeover.go",
	} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		bodies[file] = string(b)
	}

	var offenders []string
	for name, file := range handlers {
		src := bodies[file]
		start := strings.Index(src, "func (h *Handlers) "+name+"(")
		if start < 0 {
			t.Errorf("handler %s not found in %s — if it was renamed, move this guard with it", name, file)
			continue
		}
		// The function body: to the next top-level func, or end of file.
		end := strings.Index(src[start+1:], "\nfunc ")
		body := src[start:]
		if end >= 0 {
			body = src[start : start+1+end]
		}
		if !strings.Contains(body, "writeActionOK(") {
			offenders = append(offenders, name+": does not answer with writeActionOK")
		}
		// The specific regression: handing back the created order again.
		if regexp.MustCompile(`writeJSONWithTrigger\(w, r, (order|result)\b`).MatchString(body) {
			offenders = append(offenders, name+": serialises the order it just created")
		}
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("%d operator action(s) went back to shipping a body nobody reads:\n  %s\n\n"+
			"These endpoints acknowledge and trigger a refresh; order state is read from the orders "+
			"list, which is the single place it comes from. If a caller genuinely needs data back, say "+
			"so here and give it a reader in the same change.",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

// cycle_mode is gone from NodeOrderResult and must not come back: it read
// "simple" for both a primes-only produce round and a consume downgrade, so
// the discrimination it looks like it offers is one it cannot make. Round 2
// had to key primeNoticeText on the swap legs instead.
func TestNodeOrderResultHasNoCycleMode(t *testing.T) {
	b, err := os.ReadFile("../engine/operator_stations.go")
	if err != nil {
		t.Fatalf("read operator_stations.go: %v", err)
	}
	if strings.Contains(string(b), `json:"cycle_mode"`) {
		t.Error(`cycle_mode is back on NodeOrderResult. It has no reader, and it cannot serve the ` +
			`one it would attract: a primes-only round and a consume downgrade both report "simple". ` +
			`Discriminate on the swap legs (see primeNoticeText), not on this.`)
	}
}
