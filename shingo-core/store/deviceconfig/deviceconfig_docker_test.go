//go:build docker

package deviceconfigstore_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	deviceconfigstore "shingocore/store/deviceconfig"
)

// TestLatestPullsReturnsNewestPerVehicle pins the read the whole tab hangs
// on: DISTINCT ON picks each vehicle's NEWEST row. A regression here shows
// stale configs per robot silently — the June-incident failure mode the
// tab exists to prevent (round-2, cobalt-heron H2). The config column's
// absence from the read is structural (Pull has no field for it): the
// matrix path cannot drag the big column even by mistake.
func TestLatestPullsReturnsNewestPerVehicle(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	ctx := context.Background()

	old := time.Now().Add(-2 * time.Hour).UTC()
	for _, v := range []string{"AMR-01", "AMR-02"} {
		testutil.MustNoErr(t, deviceconfigstore.SavePull(ctx, db.DB, deviceconfigstore.Pull{
			Vehicle: v, PulledAt: old, ModelMD5: "old",
			Params: json.RawMessage(`[{"ident":"stale"}]`),
		}, []byte(`{"row":"old"}`)), "save old "+v)
		testutil.MustNoErr(t, deviceconfigstore.SavePull(ctx, db.DB, deviceconfigstore.Pull{
			Vehicle: v, PulledAt: old.Add(time.Hour), ModelMD5: "new",
			Params: json.RawMessage(`[{"ident":"fresh"}]`),
		}, []byte(`{"row":"new"}`)), "save new "+v)
	}

	rows, err := deviceconfigstore.LatestPulls(ctx, db.DB)
	testutil.MustNoErr(t, err, "latest pulls")
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want one per vehicle", len(rows))
	}
	for _, r := range rows {
		if r.ModelMD5 != "new" {
			t.Errorf("%s latest ModelMD5 = %q, want the newer pull", r.Vehicle, r.ModelMD5)
		}
		if !jsonContains(r.Params, `"fresh"`) {
			t.Errorf("%s latest params = %s, want the newer pull's leaves", r.Vehicle, r.Params)
		}
	}
	if rows[0].Vehicle > rows[1].Vehicle {
		t.Errorf("rows not ordered by vehicle: %s before %s", rows[0].Vehicle, rows[1].Vehicle)
	}
}

func jsonContains(raw json.RawMessage, needle string) bool {
	return strings.Contains(string(raw), needle)
}
