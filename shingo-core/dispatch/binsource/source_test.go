package binsource

import (
	"testing"
	"time"

	"shingocore/domain"
)

const (
	X     = "PART-X"
	Y     = "PART-Y"
	cap10 = 10
)

// Part-X bins rank by FIFO (oldest COALESCE(loaded_at, created_at)); a partial
// keeps its original loaded_at across a return, so it's just an aged bin of X.
// Empties don't rank by age — they're fungible (grab any, stable by id).
var (
	tAncient = time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	tOld     = time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	tNew     = time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC)
)

func at(t time.Time) *time.Time { return &t }

func TestSource(t *testing.T) {
	cases := []struct {
		name      string
		cands     []Cand
		want      Want
		wantFound bool
		wantBin   int64
	}{
		// ── Drain: FIFO over bins of part X (full or partial), oldest first ──
		{
			name: "Drain_payload_exact_selects_X_not_Y",
			cands: []Cand{
				{BinID: 1, Payload: Y, UOP: cap10, Cap: cap10, LoadedAt: at(tOld), CreatedAt: tOld, ManifestConfirmed: true, Status: domain.BinStatusAvailable},
				{BinID: 2, Payload: X, UOP: cap10, Cap: cap10, LoadedAt: at(tOld), CreatedAt: tOld, ManifestConfirmed: true, Status: domain.BinStatusAvailable},
			},
			want:      Want{Payload: X, Intent: Drain},
			wantFound: true, wantBin: 2,
		},
		{
			name:  "Drain_never_a_partial_of_Y",
			cands: []Cand{{BinID: 1, Payload: Y, UOP: 5, Cap: cap10, LoadedAt: at(tOld), CreatedAt: tOld, ManifestConfirmed: true, Status: domain.BinStatusAvailable}},
			want:  Want{Payload: X, Intent: Drain},
		},
		{
			name: "Drain_oldest_wins_older_full_over_newer_partial",
			cands: []Cand{
				{BinID: 1, Payload: X, UOP: cap10, Cap: cap10, LoadedAt: at(tOld), CreatedAt: tOld, ManifestConfirmed: true, Status: domain.BinStatusAvailable},
				{BinID: 2, Payload: X, UOP: 3, Cap: cap10, LoadedAt: at(tNew), CreatedAt: tNew, Status: domain.BinStatusAvailable},
			},
			want:      Want{Payload: X, Intent: Drain},
			wantFound: true, wantBin: 1,
		},
		{
			name: "Drain_older_partial_wins_over_newer_full",
			cands: []Cand{
				{BinID: 1, Payload: X, UOP: cap10, Cap: cap10, LoadedAt: at(tNew), CreatedAt: tNew, ManifestConfirmed: true, Status: domain.BinStatusAvailable},
				{BinID: 2, Payload: X, UOP: 3, Cap: cap10, LoadedAt: at(tOld), CreatedAt: tOld, Status: domain.BinStatusAvailable},
			},
			want:      Want{Payload: X, Intent: Drain},
			wantFound: true, wantBin: 2,
		},
		{
			name:  "Drain_never_returns_empty",
			cands: []Cand{{BinID: 1, Payload: "", UOP: 0, Cap: cap10, CreatedAt: tOld, Status: domain.BinStatusAvailable}},
			want:  Want{Payload: X, Intent: Drain},
		},
		{
			name:      "Drain_staged_partial_is_visible",
			cands:     []Cand{{BinID: 1, Payload: X, UOP: 3, Cap: cap10, LoadedAt: at(tNew), CreatedAt: tNew, Status: domain.BinStatusStaged}},
			want:      Want{Payload: X, Intent: Drain},
			wantFound: true, wantBin: 1,
		},
		{
			name:  "Drain_full_requires_manifest_confirmed",
			cands: []Cand{{BinID: 1, Payload: X, UOP: cap10, Cap: cap10, LoadedAt: at(tOld), CreatedAt: tOld, ManifestConfirmed: false, Status: domain.BinStatusAvailable}},
			want:  Want{Payload: X, Intent: Drain},
		},
		{
			name:      "Drain_partial_does_not_require_manifest_confirmed",
			cands:     []Cand{{BinID: 1, Payload: X, UOP: 3, Cap: cap10, LoadedAt: at(tNew), CreatedAt: tNew, ManifestConfirmed: false, Status: domain.BinStatusAvailable}},
			want:      Want{Payload: X, Intent: Drain},
			wantFound: true, wantBin: 1,
		},
		{
			// A partial keeps its original loaded_at across a return, so the older
			// parked partial wins — this is what lets plain FIFO re-consume a kept
			// partial on its own.
			name: "multi_partial_picks_oldest",
			cands: []Cand{
				{BinID: 1, Payload: X, UOP: 3, Cap: cap10, LoadedAt: at(tOld), CreatedAt: tOld, Status: domain.BinStatusAvailable},
				{BinID: 2, Payload: X, UOP: 7, Cap: cap10, LoadedAt: at(tNew), CreatedAt: tNew, Status: domain.BinStatusAvailable},
			},
			want:      Want{Payload: X, Intent: Drain},
			wantFound: true, wantBin: 1,
		},
		{
			name:      "Drain_picks_full_when_no_partial",
			cands:     []Cand{{BinID: 1, Payload: X, UOP: cap10, Cap: cap10, LoadedAt: at(tOld), CreatedAt: tOld, ManifestConfirmed: true, Status: domain.BinStatusAvailable}},
			want:      Want{Payload: X, Intent: Drain},
			wantFound: true, wantBin: 1,
		},
		{
			name:  "claimed_excluded",
			cands: []Cand{{BinID: 1, Payload: X, UOP: cap10, Cap: cap10, LoadedAt: at(tOld), CreatedAt: tOld, ManifestConfirmed: true, Claimed: true, Status: domain.BinStatusAvailable}},
			want:  Want{Payload: X, Intent: Drain},
		},
		{
			name:  "locked_excluded",
			cands: []Cand{{BinID: 1, Payload: X, UOP: cap10, Cap: cap10, LoadedAt: at(tOld), CreatedAt: tOld, ManifestConfirmed: true, Locked: true, Status: domain.BinStatusAvailable}},
			want:  Want{Payload: X, Intent: Drain},
		},
		{
			name:  "rejected_status_excluded",
			cands: []Cand{{BinID: 1, Payload: X, UOP: cap10, Cap: cap10, LoadedAt: at(tOld), CreatedAt: tOld, ManifestConfirmed: true, Status: domain.BinStatusMaintenance}},
			want:  Want{Payload: X, Intent: Drain},
		},

		// ── Fill: a partial of X to top up (FIFO), else an empty (fungible) ──
		{
			name:  "Fill_never_a_partial_of_Y",
			cands: []Cand{{BinID: 1, Payload: Y, UOP: 5, Cap: cap10, LoadedAt: at(tOld), CreatedAt: tOld, Status: domain.BinStatusAvailable}},
			want:  Want{Payload: X, Intent: Fill},
		},
		{
			// Partial of X is taken to top up, even though the empty is older — a
			// partial of X always beats an empty (an empty isn't part-X stock).
			name: "Fill_prefers_partial_of_X_over_empty",
			cands: []Cand{
				{BinID: 1, Payload: "", UOP: 0, Cap: cap10, CreatedAt: tAncient, Status: domain.BinStatusAvailable},
				{BinID: 2, Payload: X, UOP: 5, Cap: cap10, LoadedAt: at(tNew), CreatedAt: tNew, Status: domain.BinStatusAvailable},
			},
			want:      Want{Payload: X, Intent: Fill},
			wantFound: true, wantBin: 2,
		},
		{
			name: "Fill_oldest_partial_of_X_wins",
			cands: []Cand{
				{BinID: 1, Payload: X, UOP: 4, Cap: cap10, LoadedAt: at(tOld), CreatedAt: tOld, Status: domain.BinStatusAvailable},
				{BinID: 2, Payload: X, UOP: 6, Cap: cap10, LoadedAt: at(tNew), CreatedAt: tNew, Status: domain.BinStatusAvailable},
			},
			want:      Want{Payload: X, Intent: Fill},
			wantFound: true, wantBin: 1,
		},
		{
			name:  "Fill_never_returns_full",
			cands: []Cand{{BinID: 1, Payload: X, UOP: cap10, Cap: cap10, LoadedAt: at(tOld), CreatedAt: tOld, ManifestConfirmed: true, Status: domain.BinStatusAvailable}},
			want:  Want{Payload: X, Intent: Fill},
		},
		{
			// Empties are fungible — picked by id (grab any), NOT by age: id 1 wins
			// even though it is the NEWER container.
			name: "Fill_empties_by_id_not_age",
			cands: []Cand{
				{BinID: 1, Payload: "", UOP: 0, Cap: cap10, CreatedAt: tNew, Status: domain.BinStatusAvailable},
				{BinID: 2, Payload: "", UOP: 0, Cap: cap10, CreatedAt: tAncient, Status: domain.BinStatusAvailable},
			},
			want:      Want{Payload: X, Intent: Fill},
			wantFound: true, wantBin: 1,
		},
		{
			name:      "Fill_returns_empty_when_only_empty",
			cands:     []Cand{{BinID: 1, Payload: "", UOP: 0, Cap: cap10, CreatedAt: tOld, Status: domain.BinStatusAvailable}},
			want:      Want{Payload: X, Intent: Fill},
			wantFound: true, wantBin: 1,
		},

		{
			name:  "no_candidates_returns_false",
			cands: nil,
			want:  Want{Payload: X, Intent: Drain},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, found := Source(tc.cands, tc.want)
			if found != tc.wantFound {
				t.Fatalf("found = %v, want %v (got bin %d)", found, tc.wantFound, got.BinID)
			}
			if found && got.BinID != tc.wantBin {
				t.Fatalf("selected bin %d, want %d", got.BinID, tc.wantBin)
			}
		})
	}
}
