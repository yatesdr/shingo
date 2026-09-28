package www

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"shingocore/dispatch/binresolver"
)

// TestNodeAlgorithmVocabularyMatchesResolver pins the nodes page to the
// resolver's constants: the retrieve/store algorithm codes the page offers and
// the node-property keys it reads and writes.
//
// Both sides were hand-typed. A code the page offers and the resolver does not
// know falls through to the resolver's default (FIFO / LKND) with no error, so
// an operator picks an algorithm and gets a different one; a key the page
// writes under a different spelling is stored, read by nothing, and the group
// runs its default forever.
func TestNodeAlgorithmVocabularyMatchesResolver(t *testing.T) {
	t.Parallel()
	html := readFile(t, filepath.Join("templates", "nodes.html"))
	js := readFile(t, filepath.Join("static", "pages", "nodes-detail.js"))

	optionValues := func(selectID string) []string {
		_, rest, ok := strings.Cut(html, `<select id="`+selectID+`">`)
		if !ok {
			t.Fatalf("nodes.html: <select id=%q> not found", selectID)
		}
		body, _, _ := strings.Cut(rest, "</select>")
		var out []string
		for _, m := range regexp.MustCompile(`<option value="([^"]*)"`).FindAllStringSubmatch(body, -1) {
			out = append(out, m[1])
		}
		sort.Strings(out)
		return out
	}
	sorted := func(v ...string) []string { sort.Strings(v); return v }

	if got, want := optionValues("nf-retrieve-algo"), sorted(binresolver.RetrieveFIFO, binresolver.RetrieveCOST, binresolver.RetrieveFAVL); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("nodes.html retrieval options = %v, resolver codes = %v", got, want)
	}
	if got, want := optionValues("nf-store-algo"), sorted(binresolver.StoreLKND, binresolver.StoreDPTH); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("nodes.html storage options = %v, resolver codes = %v", got, want)
	}

	for _, key := range []string{binresolver.PropRetrieveAlgorithm, binresolver.PropStoreAlgorithm, binresolver.PropResolveAround} {
		if !strings.Contains(js, "key: '"+key+"'") {
			t.Errorf("nodes-detail.js does not write property %q (resolver key) — the saved value is read by nothing", key)
		}
		if !strings.Contains(js, "p.key === '"+key+"'") {
			t.Errorf("nodes-detail.js does not read property %q back into the form", key)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
