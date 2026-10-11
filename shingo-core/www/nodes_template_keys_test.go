package www

import (
	"os"
	"regexp"
	"sort"
	"testing"
)

// TestNodesPage_TheHandlerProvidesEveryKeyTheTemplateReads pins the page's
// contract by its two halves. The nodes template reads its data as `$.Key`;
// handleNodes builds that data as a map. A key the template reads that the
// handler never sets renders as nil, and `index` of nil stops the whole page
// at that tile: since C2b's waiting-spots change the page rendered no group
// and no lane after the first group tile ("index of untyped nil", seen on the
// rig 2026-10-11). The render tests build their own map, so they could not see
// it.
func TestNodesPage_TheHandlerProvidesEveryKeyTheTemplateReads(t *testing.T) {
	tmpl, err := os.ReadFile("templates/nodes.html")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	handler, err := os.ReadFile("handlers_nodes.go")
	if err != nil {
		t.Fatalf("read handler: %v", err)
	}
	reads := map[string]bool{}
	for _, m := range regexp.MustCompile(`\$\.([A-Z][A-Za-z0-9]*)`).FindAllStringSubmatch(string(tmpl), -1) {
		reads[m[1]] = true
	}
	var missing []string
	for k := range reads {
		if !regexp.MustCompile(`"` + k + `"\s*:`).Match(handler) {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("nodes.html reads %v, which handleNodes never sets: the page stops at the first tile that reads it", missing)
	}
}
