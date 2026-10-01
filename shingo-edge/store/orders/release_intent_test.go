package orders

import (
	"testing"

	"shingo/protocol/testutil"
)

// TestListReleaseIntentNodes_OnlyUnsentIntents pins the intent worker's sweep
// (boot and the 15 s floor) to the intents it can act on. A sent intent stays on
// the row until Core stages the leg again or it ends, so a sweep over every
// intent would visit nearly every released order's node each 15 s to find
// nothing; an ended order is never swept.
func TestListReleaseIntentNodes_OnlyUnsentIntents(t *testing.T) {
	db := openWindowDB(t)
	for _, r := range []struct {
		uuid, status, intent string
		node                 int64
	}{
		{"held", "staged", `{"station_wait":0,"purpose":"swap"}`, 1},
		{"sent", "in_transit", `{"station_wait":0,"purpose":"swap","sent_at":"2026-10-01T00:00:00Z"}`, 2},
		{"none", "staged", ``, 3},
		{"ended", "confirmed", `{"station_wait":0,"purpose":"swap"}`, 4},
	} {
		_, err := db.Exec(`INSERT INTO orders (uuid, status, process_node_id, release_intent) VALUES (?, ?, ?, ?)`,
			r.uuid, r.status, r.node, r.intent)
		testutil.MustNoErr(t, err, "insert "+r.uuid)
	}
	got, err := ListReleaseIntentNodes(db)
	testutil.MustNoErr(t, err, "ListReleaseIntentNodes")
	if len(got) != 1 || got[0] != 1 {
		t.Errorf("swept nodes %v, want [1]: only an unsent intent on a live order has work for the floor", got)
	}
}
