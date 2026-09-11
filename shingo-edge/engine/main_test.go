package engine

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain deletes the database template testEngineDB builds once per test
// binary. Nothing else removes it, so every run of this package used to leave
// a template directory in the OS temp dir. It returns rather than calling
// os.Exit so that cleanup runs; the test binary still exits with m.Run's code.
func TestMain(m *testing.M) {
	m.Run()
	if engineDBTemplatePath != "" {
		_ = os.RemoveAll(filepath.Dir(engineDBTemplatePath))
	}
}
