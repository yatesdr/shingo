package store

import "testing"

// The invalid_state rollback returned the order to staged and rendered NO CHIP
// AT ALL: the board keyed on the manifest-sync prefix alone, and the rejection
// sentence does not start with it. The order reappeared in the active list with
// nothing to say why it had come back.
func TestIsReleaseErrorDetail(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		detail string
		want   bool
		why    string
	}{
		{"Manifest sync failed at Core: locked bin. Click release to retry.", true,
			"the original chip class must keep rendering"},
		{"Core rejected the release: leg is not releasable. Blocked by: locked bin (locked). Release again once that clears.", true,
			"the invalid_state rollback is the class that rendered nothing"},
		{"Core rejected the release. Click release to retry.", true,
			"the rejection sentence with no Core detail still keys the chip"},
		{"order dispatched", false,
			"an ordinary transition is not a release error"},
		{"", false, "an empty detail is not a release error"},
	} {
		tc := tc
		t.Run(tc.detail, func(t *testing.T) {
			t.Parallel()
			if got := isReleaseErrorDetail(tc.detail); got != tc.want {
				t.Errorf("isReleaseErrorDetail(%q) = %v, want %v — %s", tc.detail, got, tc.want, tc.why)
			}
		})
	}
}
