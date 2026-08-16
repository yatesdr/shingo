//go:build docker

package testdb

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/testcontainers/testcontainers-go"
)

// reapProbeImage is the image the planted containers use. It is the same image
// the harness already pulls, so this test adds no download; the entrypoint is
// overridden to `sleep` so nothing runs initdb — the reaper decides on labels
// alone and does not care what is inside.
const reapProbeImage = "postgres:16-alpine"

// probeOwner returns a labelOwner value unique to this test, so the fixtures it
// plants are invisible to every other reaper on the machine.
//
// WHY THIS IS NOT OPTIONAL. These tests plant a deliberately-EXPIRED container
// and then assert the reaper collects it. Under `go test ./...` the packages run
// concurrently against one Docker daemon, and every sibling that starts a
// database calls reapOrphansBestEffort BEFORE creating its own container
// (testdb.go) — doing exactly what it is supposed to do, to a fixture that looks
// exactly like the debris it exists to clear. The fixture vanishes between being
// planted and being listed, and the assertion fails with removed=[] on a reaper
// that is working perfectly.
//
// That is a genuine race on a global resource, not flakiness to be retried: it
// passed in isolation and failed in the full suite, on the parent commit as well
// as on the branch that surfaced it. Scoping the fixture removes the shared
// resource from the equation rather than narrowing the window.
func probeOwner(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("probe-%d-%s", os.Getpid(), strings.ReplaceAll(t.Name(), "/", "_"))
}

// plantContainer starts a labelled container and registers its forced removal.
// It returns the ID. Cleanup is unconditional so a failing assertion cannot
// leave the very debris this file is about.
func plantContainer(t *testing.T, labels map[string]string) string {
	t.Helper()
	ctx := context.Background()
	cli, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		t.Skipf("skipping reaper test: docker client: %v", err)
	}
	defer cli.Close()

	created, err := cli.ContainerCreate(ctx, &container.Config{
		Image:      reapProbeImage,
		Entrypoint: []string{"sleep"},
		Cmd:        []string{"600"},
		Labels:     labels,
	}, nil, nil, nil, "")
	if err != nil {
		t.Skipf("skipping reaper test: create probe container: %v", err)
	}
	t.Cleanup(func() {
		cleanupCli, err := testcontainers.NewDockerClientWithOpts(context.Background())
		if err != nil {
			return
		}
		defer cleanupCli.Close()
		_ = cleanupCli.ContainerRemove(context.Background(), created.ID,
			container.RemoveOptions{Force: true, RemoveVolumes: true})
	})
	if err := cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		t.Fatalf("start probe container: %v", err)
	}
	return created.ID
}

// containerExists reports whether id is still known to Docker in any state.
func containerExists(t *testing.T, id string) bool {
	t.Helper()
	ctx := context.Background()
	cli, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()
	if _, err := cli.ContainerInspect(ctx, id); err != nil {
		return false
	}
	return true
}

// TestReapOrphans_RemovesExpiredAndSparesLive is the whole safety claim in one
// test, and both halves have to hold — a reaper that removes nothing is
// useless, and one that removes a container another test process is using is
// worse than the debris, because the victim fails somewhere unrelated as a
// query error.
//
// Three containers are planted, differing ONLY in the deadline label:
//
//	expired      — deadline in the past      → must be removed
//	live         — deadline in the future    → must survive
//	noDeadline   — our owner label, no deadline label → must survive
//
// The third is the fail-closed case and is the one most likely to regress: an
// implementation that treats a missing deadline as zero time reads it as
// infinitely expired and deletes it. That is a live container belonging to a
// `go test -timeout=0` run, which is precisely the run nothing can prove dead.
func TestReapOrphans_RemovesExpiredAndSparesLive(t *testing.T) {
	now := time.Now()
	owner := probeOwner(t)

	expired := plantContainer(t, map[string]string{
		labelOwner:    owner,
		labelDeadline: now.Add(-1 * time.Minute).Format(time.RFC3339Nano),
	})
	live := plantContainer(t, map[string]string{
		labelOwner:    owner,
		labelDeadline: now.Add(30 * time.Minute).Format(time.RFC3339Nano),
	})
	noDeadline := plantContainer(t, map[string]string{
		labelOwner: owner,
	})

	ctx, cancel := context.WithTimeout(context.Background(), reapTimeout)
	defer cancel()
	removed, err := reapOrphans(ctx, now, owner)
	if err != nil {
		t.Fatalf("reapOrphans: %v", err)
	}

	var sawExpired bool
	for _, id := range removed {
		if id == live || id == noDeadline {
			t.Errorf("reapOrphans removed a container it must not have: %s", id[:12])
		}
		if id == expired {
			sawExpired = true
		}
	}
	if !sawExpired {
		t.Errorf("reapOrphans did not report removing the expired container %s; removed=%v", expired[:12], removed)
	}

	// Assert against Docker rather than the return value alone: the returned
	// slice is this code's claim about what it did, and the container list is
	// what actually happened.
	if containerExists(t, expired) {
		t.Errorf("expired container %s still exists after reap", expired[:12])
	}
	if !containerExists(t, live) {
		t.Errorf("live container %s was removed by the reap", live[:12])
	}
	if !containerExists(t, noDeadline) {
		t.Errorf("container %s with no deadline label was removed by the reap", noDeadline[:12])
	}
}

// TestReapOrphans_IgnoresContainersItDoesNotOwn pins the candidate filter. A
// container without the owner label must not even be listed, whatever else is
// on it — the deadline label alone is not a licence to delete, or a stray label
// on somebody's unrelated container becomes a way to have it deleted.
func TestReapOrphans_IgnoresContainersItDoesNotOwn(t *testing.T) {
	now := time.Now()
	foreign := plantContainer(t, map[string]string{
		labelDeadline: now.Add(-1 * time.Hour).Format(time.RFC3339Nano),
	})

	ctx, cancel := context.WithTimeout(context.Background(), reapTimeout)
	defer cancel()
	removed, err := reapOrphans(ctx, now, probeOwner(t))
	if err != nil {
		t.Fatalf("reapOrphans: %v", err)
	}
	for _, id := range removed {
		if id == foreign {
			t.Fatalf("reapOrphans removed %s, which carries no %s label", foreign[:12], labelOwner)
		}
	}
	if !containerExists(t, foreign) {
		t.Fatalf("container %s without the %s label was removed", foreign[:12], labelOwner)
	}
}

// TestSharedContainer_CarriesReapLabels reads the labels back off the container
// this process actually created. Stamping them in startContainer and asserting
// only on containerLabels() would test the map builder, not the container —
// same failure family as a column written everywhere and read nowhere. If
// CustomizeRequest ever stops merging labels, the reaper silently degrades to
// reaping nothing and every other test here still passes, because they plant
// their own labels.
func TestSharedContainer_CarriesReapLabels(t *testing.T) {
	// Nothing to read the labels off when the server was handed to us: on the
	// $SHINGO_TEST_PG path this process creates no container, and an empty
	// ContainerID() is the correct answer rather than the failure this test
	// looks for. Skip rather than relax the assertion — the assertion is the
	// whole point of the test on the path where a container does exist, and
	// scripts/gate.sh runs both paths across a full gate.
	if addr := os.Getenv(envSharedPG); addr != "" {
		t.Skipf("using the shared server at %s; this process creates no container to label", addr)
	}
	_ = Open(t) // forces containerOnce
	id := ContainerID()
	if id == "" {
		t.Fatal("ContainerID() is empty after Open; startContainer did not record it")
	}

	ctx := context.Background()
	cli, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()
	info, err := cli.ContainerInspect(ctx, id)
	if err != nil {
		t.Fatalf("inspect shared container %s: %v", id[:12], err)
	}

	if info.Config.Labels[labelOwner] != ownerHarness {
		t.Errorf("shared container missing %s=%s; labels=%v", labelOwner, ownerHarness, info.Config.Labels)
	}
	raw, ok := info.Config.Labels[labelDeadline]
	if !ok {
		t.Fatalf("shared container missing %s; nothing will ever reap it", labelDeadline)
	}
	deadline, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatalf("shared container %s=%q does not parse as RFC3339Nano: %v", labelDeadline, raw, err)
	}
	// The suite always runs under a -timeout, so the deadline must be ahead of
	// now — a container reapable while its own tests are still running is the
	// failure this whole design is built to exclude.
	if !deadline.After(time.Now()) {
		t.Errorf("shared container deadline %s is not in the future; this process can be reaped mid-run", raw)
	}
}

// TestContainerDeadline_DerivesFromTestTimeout pins the derivation rather than
// the value. The number itself is whatever -timeout the run was given, so
// asserting a constant would pass trivially under one invocation and fail under
// another; what must hold across every invocation is that the deadline sits
// beyond the timeout the binary is running under, by exactly the declared
// slack.
func TestContainerDeadline_DerivesFromTestTimeout(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	got := containerDeadline(now)
	if got.IsZero() {
		t.Skip("this binary is running without -test.timeout; no deadline to derive")
	}
	gap := got.Sub(now)
	if gap <= reapSlack {
		t.Fatalf("deadline gap %s does not exceed reapSlack %s; timeout was not added", gap, reapSlack)
	}
	if _, ok := containerLabels(now)[labelDeadline]; !ok {
		t.Fatalf("containerLabels omitted %s while containerDeadline returned %s", labelDeadline, got)
	}
}
