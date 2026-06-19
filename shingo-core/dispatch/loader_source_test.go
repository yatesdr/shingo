package dispatch

import (
	"testing"
	"time"

	"shingocore/store/bins"
)

// Pure (no Postgres): candFromBin is a field map, so it is unit-tested without
// the docker tag — it pins the bins.Bin -> binsource.Cand contract the ranker
// relies on.
func TestCandFromBin(t *testing.T) {
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
