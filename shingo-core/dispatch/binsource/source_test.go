package binsource

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingocore/domain"
)

const (
	X     = "PART-X"
	Y     = "PART-Y"
	cap10 = 10
)

// Part-X bins rank by FIFO (oldest COALESCE(loaded_at, created_at)); a partial
// keeps its original loaded_at across a return, so it's just an aged bin of X.
// Empties don't rank by age: with no height wanted they're fungible (grab any,
// stable by id); with one they rank on its ladder (TestSourceFillRanksEmptiesByTheLadder).
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
			got, found := Source(stocked(tc.cands), tc.want)
			if found != tc.wantFound {
				t.Fatalf("found = %v, want %v (got bin %d)", found, tc.wantFound, got.BinID)
			}
			if found && got.BinID != tc.wantBin {
				t.Fatalf("selected bin %d, want %d", got.BinID, tc.wantBin)
			}
		})
	}
}

// stocked is what the caller reads off each row before the selector sees it
// (candFromBin: domain.Bin.Stocked): the table states its candidates' counts.
func stocked(cands []Cand) []Cand {
	out := make([]Cand, len(cands))
	for i, c := range cands {
		c.Stocked = c.UOP > 0
		out[i] = c
	}
	return out
}

// TestSourceFillRanksEmptiesByTheLadder: a Fill wanted at a height of n layers
// takes the empties in the ladder's order (the SQL twin is bins'
// preferredEmptyOrder; the agreement table over both is
// TestHeightLadder_ThePoolAgreesWithTheFinders): a known count before an
// unknown one, at least the want before fewer, nearest first, then by id. At 0
// that is tops & bottoms first, then the shortest stack. A want that states no
// layers ranks no height. Every pool below lands the carts in the reverse of
// the ladder, so ranking by id alone takes the other one.
func TestSourceFillRanksEmptiesByTheLadder(t *testing.T) {
	n := func(v int) *int { return &v }
	empty := func(id int64, totes, perLayer *int) Cand {
		return Cand{BinID: id, Cap: cap10, CreatedAt: tOld, Status: domain.BinStatusAvailable,
			TotesOnCart: totes, TotesPerLayer: perLayer, State: protocol.CarrierStateOf("", false, totes, perLayer)}
	}
	partial := Cand{BinID: 2, Payload: X, UOP: 5, Stocked: true, Cap: cap10, LoadedAt: at(tNew), CreatedAt: tNew,
		Status: domain.BinStatusAvailable, State: protocol.CarrierLoaded}
	for _, c := range []struct {
		name   string
		cands  []Cand
		height domain.Height
		want   int64
	}{
		{"at 0, tops & bottoms ahead of a two-high", []Cand{empty(1, n(16), n(8)), empty(2, n(0), n(8))}, protocol.Layers(0), 2},
		{"at 0, the shortest stack", []Cand{empty(1, n(16), n(8)), empty(2, n(8), n(8))}, protocol.Layers(0), 2},
		{"the exact height", []Cand{empty(1, n(8), n(8)), empty(2, n(16), n(8))}, protocol.Layers(2), 2},
		{"the shortest taller before the tallest shorter", []Cand{empty(1, n(8), n(8)), empty(2, n(24), n(8))}, protocol.Layers(2), 2},
		{"a shorter one before a count not known", []Cand{empty(1, nil, n(8)), empty(2, n(8), n(8))}, protocol.Layers(2), 2},
		{"a shorter one before a type with no totes per layer", []Cand{empty(1, n(3), nil), empty(2, n(8), n(8))}, protocol.Layers(2), 2},
		{"one rung: by id", []Cand{empty(1, n(16), n(8)), empty(2, n(16), n(8))}, protocol.Layers(2), 1},
		{"no height said: by id", []Cand{empty(1, n(8), n(8)), empty(2, n(16), n(8))}, domain.Height{}, 1},
		{"tops & bottoms only: by id", []Cand{empty(1, n(0), n(8)), empty(2, n(0), n(8))}, protocol.TopsAndBottomsOnly, 1},
		{"a partial of the part before any empty", []Cand{empty(1, n(16), n(8)), partial}, protocol.Layers(2), 2},
	} {
		got, ok := Source(c.cands, Want{Payload: X, Intent: Fill, Height: c.height})
		if !ok || got.BinID != c.want {
			t.Errorf("%s: took %d (found %v), want %d", c.name, got.BinID, ok, c.want)
		}
	}
}
