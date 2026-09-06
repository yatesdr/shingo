package clock

import (
	"fmt"
	"os"
	"time"
)

// AnchorEnv is the environment variable carrying the run's shared sim anchor,
// as an RFC3339 instant. It is read IDENTICALLY by Core, Edge and the seeder
// through ResolveAnchor, for the same reason BuildSimClock is shared: the
// anchor is only useful if every process in the run computes simulated time
// from the same one, and the way to guarantee that is to have one function.
const AnchorEnv = "SHINGO_SIM_ANCHOR"

// ResolveAnchor decides the (epoch, anchorWall) pair the sim clock is built
// from, preferring the run's environment anchor over whatever the config file
// carries, and reporting which source won so the caller can say so in its
// banner.
//
// ── WHY THE YAML CANNOT OWN THIS NUMBER ───────────────────────────────────
//
// A sim clock computes `sim_now = epoch + speed × (wallNow − anchorWall)`. The
// dev configs set both to the same hardcoded calendar date, which makes the
// second term grow without bound against a fixed origin: the offset between
// simulated and real time is `(speed − 1) × (wallNow − anchorWall)`, so it
// widens by (speed−1) days per real day, forever, whether or not the stack has
// ever been started. At 2× and a date eight weeks old the sim was running two
// months into the future; at 10× it was a year and five months ahead. Nothing
// resets it, because a checked-in literal has no relationship to when the run
// began.
//
// That is not cosmetic. Simulated timestamps are compared against browser
// wall-time on every page (`Date.now() − since` for the live durations), so
// every elapsed reading on the rig rendered 0 — the one instrument that
// distinguishes backpressure from a wedge, dead. And a speed change re-anchors
// the clock to its current simulated time, so editing the multiplier jumped the
// world by (Δspeed) × (age of the literal): the 10×→2× edit on 2026-09-05 moved
// simulated time 439 days backwards, past every timestamp already in the
// database.
//
// So the anchor belongs to THE RUN, not to the file. It is derived once when
// the stack is brought up and handed to every process through the environment,
// which is also what keeps them in lockstep — the property the shared
// `anchor_wall` key was added for and the only reason it exists.
//
// ── EPOCH AND ANCHOR MOVE TOGETHER, AND THAT PICKS THE MODE ───────────────
//
// The env carries ONE instant and it becomes both. `epoch == anchorWall` is
// what BuildSimClock reads as "start here and run fast, in lockstep"
// (SimSyncedRunning): no wall clamp, so the multiplier is genuinely sustained,
// and a shared origin, so Core and Edge agree. A fresh bring-up therefore
// starts the simulated world at today and runs it forward at `speed`.
//
// Replaying history — an epoch deliberately BEFORE its anchor — is the other
// intent, and it stays a config-file matter: leave the env unset and set
// `sim.epoch` and `sim.anchor_wall` in the YAML as before. This function then
// returns them untouched.
//
// ── THE ANCHOR'S LIFETIME IS THE DATABASE'S LIFETIME ──────────────────────
//
// Re-anchoring is only safe when the data is also fresh. Simulated timestamps
// already written to Postgres and the Edge SQLite are stamped in the OLD
// anchor's frame; minting a new anchor over a surviving volume puts simulated
// now BEFORE rows that already exist, which is the same backwards jump the
// speed edit caused. So the anchor is minted by the bring-up alongside fresh
// volumes and persists across individual service restarts (see
// scripts/sim-anchor.sh and the Makefile's dev / dev-reset targets). Restarting
// one container must never re-anchor it, which is exactly why this reads an
// environment value it does not compute.
func ResolveAnchor(epoch, anchorWall time.Time) (outEpoch, outAnchor time.Time, source string, err error) {
	raw, ok := os.LookupEnv(AnchorEnv)
	if !ok || raw == "" {
		return epoch, anchorWall, "config", nil
	}
	t, perr := time.Parse(time.RFC3339, raw)
	if perr != nil {
		// Refused, not ignored. Falling back to the config file here would
		// restore the stale-literal clock silently, under an env var whose
		// presence says somebody meant to control the anchor — and the two
		// processes might not even fail the same way. A boot failure names the
		// problem where somebody is looking.
		return time.Time{}, time.Time{}, "", fmt.Errorf(
			"%s=%q is not an RFC3339 instant (want e.g. 2026-09-06T14:00:00Z): %w",
			AnchorEnv, raw, perr)
	}
	return t.UTC(), t.UTC(), AnchorEnv, nil
}
