package domain

import "testing"

// TestSplitRequester_ActorsAndStations: each of the three Core actors is a
// requester with no station, and anything else, blank and near misses
// included, is a station's uid. IsCoreActor reads CoreActors, so this is the
// list's reader pinned: an actor dropped from the list would read as a
// station.
func TestSplitRequester_ActorsAndStations(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ sender, station, requester string }{
		{"core-operator", "", "core-operator"},
		{"core-direct", "", "core-direct"},
		{"core-test", "", "core-test"},
		{"plant.line-1", "plant.line-1", ""},
		{"", "", ""},
		{"core", "core", ""},
		{"Core-Test", "Core-Test", ""},
	} {
		station, requester := SplitRequester(c.sender)
		if station != c.station || requester != c.requester {
			t.Errorf("SplitRequester(%q) = (%q, %q), want (%q, %q)", c.sender, station, requester, c.station, c.requester)
		}
	}
	if len(CoreActors) != 3 {
		t.Errorf("CoreActors holds %d names, want the 3 v185's CHECKs allow in requested_by", len(CoreActors))
	}
}
