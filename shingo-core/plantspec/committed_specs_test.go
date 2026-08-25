package plantspec

import (
	"path/filepath"
	"testing"
)

// TestCommittedSpecsValidate runs the validator over every plant spec in the
// repository.
//
// NOTHING DID THIS BEFORE. Validate() has thorough unit tests over hand-built
// Plant values, and the specs the sim actually runs were checked only by
// running seeddev against them — which needs Postgres and SQLite up, so in
// practice a broken spec was found by a person waiting for a stack to come up.
//
// The specs are the sim's fixtures, and a fixture that cannot be seeded is a
// scenario nobody can run. This catches that in milliseconds, on every push,
// with no containers.
func TestCommittedSpecsValidate(t *testing.T) {
	t.Parallel()
	specs, err := filepath.Glob(filepath.Join("..", "..", "plants", "*.yaml"))
	if err != nil {
		t.Fatalf("glob plants: %v", err)
	}
	if len(specs) == 0 {
		t.Fatal("no plant specs found — the path is wrong, and a test that checks nothing passes")
	}
	for _, spec := range specs {
		t.Run(filepath.Base(spec), func(t *testing.T) {
			t.Parallel()
			p, err := Load(spec)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if err := p.Validate(); err != nil {
				t.Errorf("%v", err)
			}
		})
	}
}
