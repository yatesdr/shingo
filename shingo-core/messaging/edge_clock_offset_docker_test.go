//go:build docker

package messaging

import (
	"database/sql"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store"
)

func storedClockOffsetMS(t *testing.T, db *store.DB, uid string) sql.NullInt64 {
	t.Helper()
	var ms sql.NullInt64
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT clock_offset_ms FROM edge_registry WHERE station_uid = $1`, uid).Scan(&ms),
		"read the stored clock offset")
	return ms
}

// TestHeartbeat_RecordsTheEdgesClockOffset: Core keeps each Edge's clock
// offset as its heartbeat implies it, the envelope's ts (the Edge's clock when
// it built the heartbeat) minus Core's clock on receipt, in the UPDATE that
// already marks the station alive. The broker's delay is in it and reads as
// the Edge being behind by that much. A heartbeat with no ts leaves the last
// reading alone.
func TestHeartbeat_RecordsTheEdgesClockOffset(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	const uid = "stn-clock"
	_, err := db.IntroduceEdge(uid, "host-clock", "v-test")
	testutil.MustNoErr(t, err, "introduce edge")
	svc := NewCoreDataService(db, &captureResponder{}, service.EpochAnnounce{})

	env := ticksEnv(uid)
	env.Timestamp = clock.Now().Add(12 * time.Second)
	svc.HandleEdgeHeartbeat(env, &protocol.EdgeHeartbeat{StationID: uid})
	ms := storedClockOffsetMS(t, db, uid)
	if !ms.Valid || ms.Int64 < 11000 || ms.Int64 > 12000 {
		t.Fatalf("stored offset = %v ms, want about +12000 (the Edge's ts 12 s ahead of Core's receipt)", ms)
	}

	svc.HandleEdgeHeartbeat(ticksEnv(uid), &protocol.EdgeHeartbeat{StationID: uid})
	if again := storedClockOffsetMS(t, db, uid); again != ms {
		t.Errorf("a heartbeat with no ts moved the offset %v -> %v", ms, again)
	}

	env = ticksEnv(uid)
	env.Timestamp = clock.Now()
	svc.HandleEdgeHeartbeat(env, &protocol.EdgeHeartbeat{StationID: uid})
	if ms := storedClockOffsetMS(t, db, uid); !ms.Valid || ms.Int64 < -1000 || ms.Int64 > 0 {
		t.Errorf("stored offset for an Edge on Core's time = %v ms, want about 0", ms)
	}
}
