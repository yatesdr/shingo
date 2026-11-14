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
	out, err := exec.Command(nodePath, filepath.Join("testdata", "js", "scene-geom.test.js")).CombinedOutput()
	if err != nil {
		t.Fatalf("scene-geom agreement test failed:\n%s\nerror: %v", out, err)
	}
	t.Logf("scene-geom agreement: %s", out)
}

// TestSceneNetRoutesAsTheDesktopDoes runs scene-net.test.js: the scene network
// and the route model of shared/scene-net.js on the synthetic route scene. The
// route vectors are the Edge desktop process map's, whose drawing and own
// route pin were removed (2026-11-14), with the dead-end destination fixed;
// the lane list is the order and orientation Core's robot map has always drawn
// its aisles in.
func TestSceneNetRoutesAsTheDesktopDoes(t *testing.T) {
	runSceneJS(t, "scene-net.test.js")
}

// TestScenePathDrawsRDSPath runs scene-path.test.js: shared/scene-path.js
// drawing a robot's path along the stored rows (lead decision D6).
func TestScenePathDrawsRDSPath(t *testing.T) {
	runSceneJS(t, "scene-path.test.js")
}

func runSceneJS(t *testing.T, script string) {
	t.Helper()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping %s", script)
	}
	out, err := exec.Command(nodePath, filepath.Join("testdata", "js", script)).CombinedOutput()
	if err != nil {
		t.Fatalf("%s failed:\n%s\nerror: %v", script, out, err)
	}
	t.Logf("%s: %s", script, out)
}
