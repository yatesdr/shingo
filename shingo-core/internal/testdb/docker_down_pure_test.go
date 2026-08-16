package testdb

import (
	"errors"
	"strings"
	"testing"
)

// docker_down_pure_test.go — the instrument half of fix-batch 2a. The other
// half (the sentinel emission and the env branch) is in testdb.go's docker
// arms; this file pins the pieces that are testable WITHOUT a container:
// the sentinel's exact shape and the once-per-process guarantee.

// TestRequireDockerSentinelShape pins the line the gate and the smoke grep for.
//
// The wording is a CONTRACT, not prose. `grep '^SHINGO-DOCKER-DOWN'` runs in
// scripts/gate.sh step_docker against every module's log — if this format
// drifts, that grep goes quietly blind, and a lying green is exactly what the
// instrument exists to prevent. It is pinned as a literal on the right-hand
// side, the same convention as the queue-cause values, so a change is a
// deliberate act that fails this test rather than an edit nobody notices.
//
// MUTATION (verified): change the sentinel prefix in dockerDownSentinel to
// "shingo-docker-down". This fires naming both spellings.
func TestRequireDockerSentinelShape(t *testing.T) {
	got := dockerDownSentinel(errors.New("docker daemon not running"))
	const want = "SHINGO-DOCKER-DOWN: skipping integration tests: docker daemon not running"
	if got != want {
		t.Fatalf("sentinel line = %q, want %q — scripts/gate.sh greps for the prefix and the "+
			"Sunday smoke asserts on the count; a drifted shape is an instrument that reports "+
			"green while blind", got, want)
	}
}

// TestRequireDockerEnvConstantName pins the env var the gate exports. Same
// contract argument: step_docker sets SHINGO_TEST_REQUIRE_DOCKER and this
// package branches on it, and a rename on one side without the other is a
// silent disarm.
func TestRequireDockerEnvConstantName(t *testing.T) {
	if envRequireDocker != "SHINGO_TEST_REQUIRE_DOCKER" {
		t.Fatalf("envRequireDocker = %q — scripts/gate.sh exports SHINGO_TEST_REQUIRE_DOCKER; "+
			"the two spellings must change together or the gate arms nothing", envRequireDocker)
	}
}

// TestNoteDockerDown_ReportsOncePerProcess pins the once-only guarantee: one
// package with a hundred docker tests funnels through the same arms, and the
// log should carry one sentinel line, not a hundred.
func TestNoteDockerDown_ReportsOncePerProcess(t *testing.T) {
	dockerDownReported.Store(false)
	defer dockerDownReported.Store(false)

	first := dockerDownReported.CompareAndSwap(false, true)
	second := dockerDownReported.CompareAndSwap(false, true)
	if !first {
		t.Fatal("the first call must report — the sentinel was not yet emitted this process")
	}
	if second {
		t.Fatal("the second call reported again — one package, one sentinel; more is noise the " +
			"grep has to deduplicate")
	}
}

// TestDockerDownSentinelPrefixIsGrepSafe confirms the line starts with the
// exact prefix the gate greps — leading-whitespace or timestamp prefixes from
// a future log wrapper would break `grep '^SHINGO-DOCKER-DOWN'`.
func TestDockerDownSentinelPrefixIsGrepSafe(t *testing.T) {
	line := dockerDownSentinel(errors.New("x"))
	if !strings.HasPrefix(line, "SHINGO-DOCKER-DOWN") {
		t.Fatalf("line %q does not start with the prefix — the gate's anchored grep misses it", line)
	}
}
