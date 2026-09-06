package orders

import (
	"strings"
	"testing"

	"shingoedge/store/orders"
)

// The retry sentence used to be appended unconditionally by the message
// handler, which never loads the order and so cannot know whether retrying is
// possible. Telling an operator to press a button that will fail identically
// until an upstream wedge clears is worse than telling them nothing: they press
// it, it fails the same way, and the message says to press it again.
func TestReleaseRejectionDetail_NamesTheBlockerInsteadOfAdvisingRetry(t *testing.T) {
	t.Parallel()
	order := &orders.Order{
		QueueReason: "no empty slot at SMN_029",
		QueueCode:   "no_slot",
	}
	got := releaseRejectionDetail(order, "leg is not releasable")

	if !strings.Contains(got, "no empty slot at SMN_029") {
		t.Errorf("the sentence does not name the blocker Core told us about.\nGot: %q", got)
	}
	if !strings.Contains(got, "no_slot") {
		t.Errorf("the sentence drops the structured queue code.\nGot: %q", got)
	}
	if strings.Contains(got, "Click release to retry") {
		t.Errorf("the sentence still advises a retry that will be refused identically until the "+
			"blocker clears. When we know why it is blocked, say so instead.\nGot: %q", got)
	}
	if !strings.Contains(got, "Release again once that clears") {
		t.Errorf("the sentence should say what WOULD make a retry work.\nGot: %q", got)
	}
}

// When Core has given us no account of the blocker, "click release to retry" is
// honest — trying again genuinely is how you find out.
func TestReleaseRejectionDetail_AdvisesRetryWhenTheBlockerIsUnknown(t *testing.T) {
	t.Parallel()
	got := releaseRejectionDetail(&orders.Order{}, "leg is not releasable")

	if !strings.Contains(got, "Click release to retry") {
		t.Errorf("with no queue reason on the order there is nothing better to say than retry.\nGot: %q", got)
	}
	if strings.Contains(got, "Blocked by") {
		t.Errorf("named a blocker it does not have.\nGot: %q", got)
	}
}

// THE PREFIX IS LOAD-BEARING. store.releaseRejectedPrefix keys the operator
// board's release-error chip on it; if this sentence stops starting with it,
// the chip silently stops appearing and the order reappears in the active list
// with nothing to say why.
func TestReleaseRejectionDetail_KeepsTheChipPrefix(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		order *orders.Order
	}{
		{"with a blocker", &orders.Order{QueueReason: "locked bin", QueueCode: "locked"}},
		{"without a blocker", &orders.Order{}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := releaseRejectionDetail(tc.order, "rejected"); !strings.HasPrefix(got, "Core rejected the release") {
				t.Errorf("detail must begin with the chip prefix.\nGot: %q", got)
			}
		})
	}
}

// An empty Core detail must not produce a dangling colon.
func TestReleaseRejectionDetail_EmptyCoreDetailReadsCleanly(t *testing.T) {
	t.Parallel()
	got := releaseRejectionDetail(&orders.Order{}, "")
	if strings.Contains(got, ": .") || strings.Contains(got, "release: ") {
		t.Errorf("empty Core detail left a dangling separator.\nGot: %q", got)
	}
}
