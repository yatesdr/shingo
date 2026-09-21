package sourceability

import (
	"testing"
	"time"

	"shingocore/store/plantclaims"
)

// ── B7: the two-grain rate (node for cells, payload plant-wide) ──────────────
//
// The pure half of B7's pins. The DB half (read_test / tte_score_docker_test)
// proves the SQL folds rows at the right grains; these prove the COMPUTE half:
// which rate a line's projection consults, and that populating the new map
// moves nothing else in the verdict.

// TestLineTTE_PrefersNodeRateAndKeepsVerdictsIdentical pins B7's honest case:
// one node per payload. RatePerNode populated with the same number the payload
// map already held changes NO verdict field — the only visible difference is
// RateGrain naming the node grain that was previously implicit.
func TestLineTTE_PrefersNodeRateAndKeepsVerdictsIdentical(t *testing.T) {
	k := key("SNF2", "A")
	in := Inputs{
		Styles:      []plantclaims.ProcessKey{k},
		Claims:      map[plantclaims.ProcessKey][]plantclaims.ClaimRow{k: {claim("N1", "BIN-A", 0)}},
		Pool:        map[string]int{"BIN-A": 1},
		LineUOP:     map[string]int{"N1": 120},
		RatePerSec:  map[string]float64{"BIN-A": 1.0},
		RatePerNode: map[nodePayload]float64{{Node: "N1", Payload: "BIN-A"}: 1.0},
	}
	cfg := Config{YellowEnabled: true, Horizon: time.Hour}
	got := byKey(Compute(in, cfg, now))[k]
	if got.Status != StatusYellow {
		t.Fatalf("status = %q, want yellow", got.Status)
	}
	if len(got.AtRisk) != 1 {
		t.Fatalf("at-risk = %+v, want one line", got.AtRisk)
	}
	line := got.AtRisk[0]
	// Every pre-B7 field identical: same node, same payload, same UOP, same
	// rate value, same projection. Only RateGrain is new.
	if line.NodeName != "N1" || line.PayloadCode != "BIN-A" || line.UOPRemaining != 120 {
		t.Fatalf("line identity = %+v", line)
	}
	if line.RatePerSec != 1.0 || line.TimeToEmpty != 120*time.Second || !line.Known {
		t.Fatalf("projection = %+v, want 1.0 UOP/s → 120s known", line)
	}
	if line.RateGrain != "node" {
		t.Errorf("RateGrain = %q, want node", line.RateGrain)
	}
}

// TestLineTTE_FallsBackToPayloadRateWhenNodeHasNoRows pins the changeover
// case: a node with no consumption in the window (first cycle after a swap)
// still gets a projection — from the plant-wide payload rate — and the sample
// records WHICH grain it used, because a plant-wide number for one cell needs
// different scrutiny than the cell's own.
func TestLineTTE_FallsBackToPayloadRateWhenNodeHasNoRows(t *testing.T) {
	k := key("SNF2", "A")
	in := Inputs{
		Styles: []plantclaims.ProcessKey{k},
		Claims: map[plantclaims.ProcessKey][]plantclaims.ClaimRow{
			k: {claim("N1", "BIN-A", 0), claim("N2", "BIN-A", 1)},
		},
		Pool:        map[string]int{"BIN-A": 2},
		LineUOP:     map[string]int{"N1": 60, "N2": 60},
		RatePerSec:  map[string]float64{"BIN-A": 0.15},
		RatePerNode: map[nodePayload]float64{{Node: "N1", Payload: "BIN-A"}: 0.03333},
		// N2 deliberately absent from RatePerNode — its rows are outside the
		// window (or it has only ever consumed from its bucket).
	}
	cfg := Config{YellowEnabled: true, Horizon: time.Hour}
	got := byKey(Compute(in, cfg, now))[k]
	if len(got.AtRisk) != 2 {
		t.Fatalf("at-risk = %+v, want both lines", got.AtRisk)
	}
	for _, line := range got.AtRisk {
		switch line.NodeName {
		case "N1": // own rows → own rate
			if line.RateGrain != "node" || line.RatePerSec != 0.03333 {
				t.Errorf("N1 = grain %q rate %v, want node/0.03333", line.RateGrain, line.RatePerSec)
			}
			secs := 60 / 0.03333
			want := time.Duration(secs * float64(time.Second))
			if line.TimeToEmpty != want {
				t.Errorf("N1 TTE = %v, want %v", line.TimeToEmpty, want)
			}
		case "N2": // no rows → plant-wide fallback
			if line.RateGrain != "payload" || line.RatePerSec != 0.15 {
				t.Errorf("N2 = grain %q rate %v, want payload/0.15", line.RateGrain, line.RatePerSec)
			}
			secs := 60 / 0.15
			want := time.Duration(secs * float64(time.Second))
			if line.TimeToEmpty != want {
				t.Errorf("N2 TTE = %v, want %v", line.TimeToEmpty, want)
			}
		}
	}
}

// TestLineTTE_TwoCellsOnePayloadTwoRates pins the split: two nodes sharing a
// payload at different velocities get different projections from one Inputs,
// where pre-B7 both read the same plant-wide number. N1 burns slow, N2 fast;
// the fast one is at risk, the slow one is not — a distinction the single
// grain could not draw at all.
func TestLineTTE_TwoCellsOnePayloadTwoRates(t *testing.T) {
	k := key("SNF2", "A")
	in := Inputs{
		Styles: []plantclaims.ProcessKey{k},
		Claims: map[plantclaims.ProcessKey][]plantclaims.ClaimRow{
			k: {claim("N1", "BIN-A", 0), claim("N2", "BIN-A", 1)},
		},
		Pool:    map[string]int{"BIN-A": 2},
		LineUOP: map[string]int{"N1": 90, "N2": 90},
		// Plant-wide average both used to read: 0.06667.
		RatePerSec: map[string]float64{"BIN-A": 0.06667},
		RatePerNode: map[nodePayload]float64{
			{Node: "N1", Payload: "BIN-A"}: 0.03333, // slow cell
			{Node: "N2", Payload: "BIN-A"}: 0.1,     // fast cell
		},
	}
	cfg := Config{YellowEnabled: true, Horizon: 30 * time.Minute}
	got := byKey(Compute(in, cfg, now))[k]
	if got.Status != StatusYellow {
		t.Fatalf("status = %q, want yellow (N2 projects dry in-window)", got.Status)
	}
	if len(got.AtRisk) != 1 || got.AtRisk[0].NodeName != "N2" {
		t.Fatalf("at-risk = %+v, want only N2 (90/0.1 = 900s < 30min; N1 = 2700s > 30min)", got.AtRisk)
	}
	if got.AtRisk[0].RateGrain != "node" {
		t.Errorf("RateGrain = %q, want node", got.AtRisk[0].RateGrain)
	}
}

// TestLineTTE_NoProjectionNamesNoGrain pins the empty-state contract: a line
// with nothing staged, or no rate at either grain, has no projection AND no
// grain — an empty RateGrain is "no rate was consulted", not a third grain a
// scorer could misread.
func TestLineTTE_NoProjectionNamesNoGrain(t *testing.T) {
	lt := lineTTE(claim("N1", "BIN-A", 0), Inputs{
		LineUOP:     map[string]int{"N1": 0},
		RatePerSec:  map[string]float64{"BIN-A": 1.0},
		RatePerNode: map[nodePayload]float64{{Node: "N1", Payload: "BIN-A"}: 1.0},
	})
	if lt.Known || lt.RateGrain != "" {
		t.Errorf("nothing staged: %+v, want unknown with empty grain", lt)
	}
	lt = lineTTE(claim("N1", "BIN-A", 0), Inputs{
		LineUOP:     map[string]int{"N1": 50},
		RatePerSec:  map[string]float64{},
		RatePerNode: map[nodePayload]float64{},
	})
	if lt.Known || lt.RateGrain != "" {
		t.Errorf("no rate at either grain: %+v, want unknown with empty grain", lt)
	}
}
