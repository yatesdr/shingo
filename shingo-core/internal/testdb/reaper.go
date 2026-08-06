package testdb

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/testcontainers/testcontainers-go"
)

// Orphan reaping for the shared Postgres container.
//
// startContainer builds one postgres:16-alpine per test PROCESS and never
// stores the handle, so nothing in this package can terminate it. Cleanup is
// entirely testcontainers' Ryuk sidecar, which reaps a session's containers
// when that session's socket closes. Ryuk is a good mechanism and it is
// currently ON here — but it is a SINGLE one, it lives in a container of its
// own, and it cannot act for a session whose Ryuk never started. Every failure
// of it looks the same from outside: containers stay up, and the next run
// starts its own on top of them. Thirty-one packages carry docker-tagged tests
// and each is its own process, so debris is measured in containers per run,
// not one.
//
// This is the backstop: before a process creates its container, it removes the
// ones whose creators are provably gone. It is complementary to CI's `-p 1` —
// that bounds how many containers exist AT ONCE, this removes the ones that
// should not exist AT ALL. Neither substitutes for the other: `-p 1` still
// leaves debris if Ryuk misses, and reaping still faces thirty-one concurrent
// containers if nothing bounds them.
//
// HOW AN ORPHAN IS IDENTIFIED, and why it cannot be a live container.
//
// Not by image, and not by age. Both would let this process delete a container
// another test process is mid-run against — a cure strictly worse than the
// disease, because a killed suite is at least loud, whereas a Postgres yanked
// out from under a running test fails as an unrelated query error somewhere
// downstream.
//
// Instead every container this package creates DECLARES ITS OWN DEADLINE in a
// label at creation time: `shingo.testdb.deadline`, set to the creating test
// binary's own -test.timeout plus slack. That value is not a guess about other
// processes. A live container cannot outlive its creator's -timeout, because
// `go test` enforces that deadline by panicking the binary — so once the wall
// clock passes a container's declared deadline, the process that created it has
// already exited, whatever it was doing. Past-deadline therefore MEANS orphan,
// rather than merely suggesting it.
//
// Three properties keep it safe, and each is load bearing:
//
//   - Only containers carrying our own `shingo.testdb` label are candidates.
//     Ryuk, the dev compose stack and anything else on the machine are outside
//     the filter entirely — not skipped later, never listed.
//   - A candidate with no deadline label, or an unparseable one, is KEPT. The
//     absence of a deadline is not evidence of expiry; it means this process
//     cannot tell, and cannot-tell must not read as safe-to-delete.
//   - The reaper runs inside containerOnce, BEFORE startContainer creates ours.
//     Self-deletion is impossible by construction rather than by a filter, so
//     there is no ordering a future edit could get wrong that leaves a process
//     eligible to reap itself.
const (
	// labelOwner marks a container as created by this package. It is the
	// entire candidate set: nothing without it is ever listed.
	labelOwner = "shingo.testdb"

	// ownerHarness is labelOwner's value on every container the harness itself
	// creates, and the fleet reapOrphansBestEffort collects. It is a constant
	// rather than per-process on purpose: the reaper's whole job is clearing
	// containers OTHER processes abandoned, so scoping it to the current process
	// would defeat it.
	//
	// It is a PARAMETER of reapOrphans rather than a hard-coded filter so that
	// this package's own tests can plant fixtures under a different value and
	// reap them without racing the fleet. See reaper_test.go: the reaper tests
	// plant a deliberately-expired container, and any sibling package starting a
	// database at that moment runs reapOrphansBestEffort BEFORE creating its own
	// (testdb.go) — which, on one shared Docker daemon, would collect the fixture
	// out from under the test. That is not hypothetical: it is why
	// TestReapOrphans_RemovesExpiredAndSparesLive failed under `go test ./...`
	// while passing in isolation, reporting removed=[] because the container was
	// already gone when it listed.
	ownerHarness = "1"

	// labelDeadline carries RFC3339Nano wall time after which the creating
	// test binary is provably gone. Absent or unparseable means "unknown",
	// which is treated as live.
	labelDeadline = "shingo.testdb.deadline"

	// reapSlack is added to the creating binary's -test.timeout. It covers
	// the gap between go test's timeout panic and the process actually
	// exiting (stack dump, SIGQUIT escalation, container teardown), so the
	// deadline is never reached while the creator is still winding down.
	reapSlack = 5 * time.Minute

	// reapTimeout bounds the whole reap. Reaping is best effort and must
	// never be the reason a suite hangs; a slow or wedged Docker daemon
	// costs this much and no more, and then tests proceed.
	reapTimeout = 30 * time.Second
)

// containerLabels returns the labels stamped on every container this package
// creates. A zero deadline (see containerDeadline) omits the deadline label
// entirely rather than writing something a reader would have to special-case:
// no label is already the "cannot tell, keep it" case.
func containerLabels(now time.Time) map[string]string {
	labels := map[string]string{labelOwner: ownerHarness}
	if d := containerDeadline(now); !d.IsZero() {
		labels[labelDeadline] = d.Format(time.RFC3339Nano)
	}
	return labels
}

// containerDeadline computes when this process's container becomes provably
// abandoned: the test binary's own -test.timeout, plus reapSlack.
//
// Returns the zero time when there is no timeout to derive it from — either
// the flag is absent (this package linked into something that is not a test
// binary) or it is explicitly 0, meaning `go test -timeout=0`, an unbounded
// run. Nothing can prove such a container's creator is gone, so it is stamped
// with no deadline and no other process will ever reap it. That is the correct
// answer rather than a gap: the alternative is picking a number on its behalf,
// which is exactly the guess this design exists to avoid.
func containerDeadline(now time.Time) time.Time {
	f := flag.Lookup("test.timeout")
	if f == nil {
		return time.Time{}
	}
	g, ok := f.Value.(flag.Getter)
	if !ok {
		return time.Time{}
	}
	d, ok := g.Get().(time.Duration)
	if !ok || d <= 0 {
		return time.Time{}
	}
	return now.Add(d + reapSlack)
}

// reapOrphans removes every container this package created whose declared
// deadline has passed. Returns the IDs it removed so a test can assert on
// them; errors are joined rather than returned on the first failure, because
// one undeletable container must not shield the rest.
//
// Exited containers are included (All: true) — they hold their anonymous data
// volume until removed, which is the ~80MB per package the Makefile's
// clean-testcontainers target exists to recover.
// owner selects the candidate set: only containers carrying labelOwner=owner are
// listed. Production passes ownerHarness; the reaper's own tests pass a
// per-run value so their fixtures are invisible to a concurrently-starting
// sibling's reap.
func reapOrphans(ctx context.Context, now time.Time, owner string) ([]string, error) {
	cli, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		return nil, fmt.Errorf("reap: docker client: %w", err)
	}
	defer cli.Close()

	found, err := cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", labelOwner+"="+owner)),
	})
	if err != nil {
		return nil, fmt.Errorf("reap: list containers: %w", err)
	}

	var removed []string
	var errs []error
	for _, c := range found {
		raw, ok := c.Labels[labelDeadline]
		if !ok {
			continue // no declared deadline: cannot tell, so keep.
		}
		deadline, parseErr := time.Parse(time.RFC3339Nano, raw)
		if parseErr != nil {
			errs = append(errs, fmt.Errorf("reap: container %s has unparseable %s=%q: %w", c.ID[:12], labelDeadline, raw, parseErr))
			continue // unparseable is also cannot-tell, so keep.
		}
		if !now.After(deadline) {
			continue // its creator may still be running.
		}
		if rmErr := cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{
			Force:         true,
			RemoveVolumes: true,
		}); rmErr != nil {
			errs = append(errs, fmt.Errorf("reap: remove %s: %w", c.ID[:12], rmErr))
			continue
		}
		removed = append(removed, c.ID)
	}
	return removed, errors.Join(errs...)
}

// reapOrphansBestEffort is the production entry point: it reports to stderr and
// never fails the run. A reap that cannot happen is a tidiness problem; a reap
// that stops the suite would be a worse one than the debris it is clearing.
// Writes go straight to os.Stderr rather than through the standard logger,
// which test code elsewhere in this repo has been observed to redirect.
func reapOrphansBestEffort() {
	ctx, cancel := context.WithTimeout(context.Background(), reapTimeout)
	defer cancel()

	removed, err := reapOrphans(ctx, time.Now(), ownerHarness)
	if len(removed) > 0 {
		fmt.Fprintf(os.Stderr, "testdb: reaped %d orphaned container(s) past their declared deadline\n", len(removed))
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "testdb: orphan reap incomplete: %v\n", err)
	}
}
