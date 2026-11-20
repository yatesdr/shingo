package shared

import "testing"

// TestCountJS runs count.test.js: the words a typed count box shows
// (count-words.js). Skipped if `node` is not on PATH, like the other JS
// wrappers.
func TestCountJS(t *testing.T) {
	runSceneJS(t, "count.test.js")
}
