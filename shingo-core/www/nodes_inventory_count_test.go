package www

import "testing"

// TestNodeInventoryCountJS runs the Node-based test of the node modal's
// inventory cell on the Nodes page (testdata/static/pages/
// nodes-detail.inventory.test.js): a bin's count, and above its part's ratio
// of 1 its whole UoP with its parts beside it. Skipped if `node` is not on
// PATH, matching the other JS test wrappers. Not named _js_test.go: Go reads
// that as GOOS=js.
func TestNodeInventoryCountJS(t *testing.T) {
	runPageNodeTest(t, "nodes-detail.inventory.test.js", "node modal inventory")
}
