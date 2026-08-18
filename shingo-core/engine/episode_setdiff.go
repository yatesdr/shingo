package engine

import "sort"

// staleEpisodeKeys is THE set difference behind every "close what the
// configuration no longer calls for" path, and there is exactly one of it.
//
// Three sites ask the identical question — the threshold notification path
// (closeThresholdEpisodesForPayloadNotIn), the threshold reconciling sweep
// (reconcileThresholdBindings), and the maintainer's config-withdrawn pass
// (closeWithdrawn) — and they must not be able to answer it differently. Two
// implementations of a set difference look identical the day they are written
// and drift the first time one of them learns something the other doesn't.
//
// The scoping subtlety that lives here is exactly that kind of lesson:
// COMPARE LIVE KEYS, never "this payload has no bindings left". The obvious
// version only closes when the rebuild comes back empty, which misses the case
// that actually happens — a payload bound at two stations loses one of them,
// the set is still non-empty, and the removed station's episode is stranded.
// Comparing keys covers both and costs nothing extra. It was learned once, on
// the threshold side, and the maintainer inherits it rather than re-learning it
// on a plant floor.
//
// What the callers legitimately differ on is only WHERE THE CANDIDATES COME
// FROM and what they do with the answer. The notification path knows which
// payload it just rebuilt and reads the monitor's own hold; the sweep knows
// nothing and reads the database; the maintainer reads the open maintain
// episodes. Those are a parameter and a return value, not a second function.
//
// A READ FAILURE MUST NEVER REACH HERE AS AN EMPTY `live`. Every caller returns
// early on a failed read, because an empty live set means "config calls for
// nothing" and this function would faithfully close every open episode in the
// plant on a transient Postgres blip — the worst possible failure mode, and one
// that would look exactly like a very effective sweep.
//
// SORTED, so a pass that closes several at once logs them in a stable order.
// Map iteration is randomised, and a log that reorders itself between passes is
// harder to diff than it needs to be.
func staleEpisodeKeys[T any](candidates map[string]T, live map[string]bool) []string {
	var stale []string
	for key := range candidates {
		if !live[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	return stale
}
