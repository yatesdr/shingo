package plan

import (
	"testing"
	"time"

	"shingocore/dispatch/binsource"
	"shingocore/store/bins"
)

// Pure (no Postgres): candFromBin is a field map, so it is unit-tested without
// the docker tag — it pins the bins.Bin -> binsource.Cand contract the ranker
// relies on.
func TestCandFromBin(t *testing.T) {
	t.Parallel()

	loaded := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	created := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	var order int64 = 77

	t.Run("partial maps fill fields and preserves loaded_at", func(t *testing.T) {
		b := &bins.Bin{
			ID: 1, PayloadCode: "PART-X", UOPRemaining: 5, UOPCapacity: 10,
			LoadedAt: &loaded, CreatedAt: created,
			ManifestConfirmed: true, Status: "staged",
		}
		got := candFromBin(b)
		if got.BinID != 1 || got.Payload != "PART-X" || got.UOP != 5 || got.Cap != 10 {
			t.Fatalf("identity/fill fields wrong: %+v", got)
		}
		if got.LoadedAt == nil || !got.LoadedAt.Equal(loaded) {
			t.Fatalf("LoadedAt not preserved: %v", got.LoadedAt)
		}
		if !got.CreatedAt.Equal(created) {
			t.Fatalf("CreatedAt wrong: %v", got.CreatedAt)
		}
		if got.Claimed || got.Locked || !got.ManifestConfirmed {
			t.Fatalf("flags wrong: %+v", got)
		}
		if got.Status != "staged" {
			t.Fatalf("status not carried: %q", got.Status)
		}
	})

	t.Run("empty bin maps to empty payload and nil loaded_at", func(t *testing.T) {
		b := &bins.Bin{
			ID: 2, PayloadCode: "", UOPRemaining: 0, UOPCapacity: 10,
			LoadedAt: nil, CreatedAt: created, Status: "available",
		}
		got := candFromBin(b)
		if got.Payload != "" {
			t.Fatalf("empty payload not mapped: %q", got.Payload)
		}
		if got.LoadedAt != nil {
			t.Fatalf("empty LoadedAt should stay nil (falls back to created_at), got %v", got.LoadedAt)
		}
	})

	t.Run("claimed bin derives Claimed from ClaimedBy", func(t *testing.T) {
		b := &bins.Bin{
			ID: 3, PayloadCode: "PART-X", UOPRemaining: 10, UOPCapacity: 10,
			LoadedAt: &loaded, CreatedAt: created, ClaimedBy: &order,
			ManifestConfirmed: true, Status: "available",
		}
		got := candFromBin(b)
		if !got.Claimed {
			t.Fatalf("ClaimedBy set but Claimed=false: %+v", got)
		}
	})
}

// Pure (no Postgres): liftWhereTheReturnLands keeps FIFO's pick when it stands
// on a spot the returning carrier can take, takes the oldest eligible bin on
// such a spot otherwise, and falls back to FIFO's pick when there is none.
func TestLiftWhereTheReturnLands(t *testing.T) {
	t.Parallel()
	const home, buffer, otherHome = int64(10), int64(20), int64(30)
	day := func(d int) *time.Time { v := time.Date(2026, 10, d, 9, 0, 0, 0, time.UTC); return &v }
	bin := func(id int64, node int64, part string, loaded *time.Time) *bins.Bin {
		n := node
		return &bins.Bin{ID: id, NodeID: &n, PayloadCode: part, UOPRemaining: 40, UOPCapacity: 100,
			LoadedAt: loaded, ManifestConfirmed: true, Status: "available"}
	}
	pool := func(bs ...*bins.Bin) ([]binsource.Cand, map[int64]*bins.Bin) {
		var cands []binsource.Cand
		byID := map[int64]*bins.Bin{}
		for _, b := range bs {
			cands = append(cands, candFromBin(b))
			byID[b.ID] = b
		}
		return cands, byID
	}
	want := binsource.Want{Payload: "PART-IN", Intent: binsource.Drain}
	pick := func(cands []binsource.Cand, byID map[int64]*bins.Bin, liftFrom []int64) int64 {
		fifo, ok := binsource.Source(cands, want)
		if !ok {
			t.Fatal("no FIFO pick")
		}
		return liftWhereTheReturnLands(cands, byID, liftFrom, want, fifo).BinID
	}

	// The 4x2 morning: the incoming part's oldest cart in its pinned home, its
	// other cart in a buffer; the return can only land on the buffer.
	cands, byID := pool(bin(1, home, "PART-IN", day(19)), bin(2, buffer, "PART-IN", day(22)))
	if got := pick(cands, byID, []int64{buffer}); got != 2 {
		t.Errorf("FIFO's bin on a spot the return cannot take: picked bin %d, want the buffer's bin 2", got)
	}
	if got := pick(cands, byID, []int64{home, buffer}); got != 1 {
		t.Errorf("FIFO's bin already on a spot the return can take: picked bin %d, want FIFO's bin 1", got)
	}
	// FIFO still ranks the spots that work.
	cands, byID = pool(bin(1, home, "PART-IN", day(19)), bin(2, buffer, "PART-IN", day(22)), bin(3, otherHome, "PART-IN", day(20)))
	if got := pick(cands, byID, []int64{buffer, otherHome}); got != 3 {
		t.Errorf("two spots that work: picked bin %d, want the older of them, bin 3", got)
	}
	// Nothing eligible on a spot that works: FIFO's own pick, and the return waits.
	cands, byID = pool(bin(1, home, "PART-IN", day(19)), bin(2, buffer, "PART-OTHER", day(18)))
	if got := pick(cands, byID, []int64{buffer}); got != 1 {
		t.Errorf("no eligible bin on a spot that works: picked bin %d, want FIFO's bin 1", got)
	}
}
