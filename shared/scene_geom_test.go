package shared

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestSceneGeomAgreesWithTheRecordedVectors runs the Node agreement test for
// shared/scene-geom.js against scene-geom.vectors.json — the output of the
// Core copy it was promoted from, recorded at the promoting commit. A vector
// that no longer matches is a behaviour change to both surfaces that draw the
// plant. Skipped if `node` is not on PATH (matches the other JS wrappers).
func TestSceneGeomAgreesWithTheRecordedVectors(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping scene-geom agreement test")
	}
	out, err := exec.Command(nodePath, filepath.Join(".", "scene-geom.test.js")).CombinedOutput()
	if err != nil {
		t.Fatalf("scene-geom agreement test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("scene-geom agreement: %s", out)
}
