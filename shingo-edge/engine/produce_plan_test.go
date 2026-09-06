package engine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// fixedNow is the wall clock every produce/consume planner test uses so
// timestamp comparisons are deterministic.
var fixedNow = time.Date(2026, 4, 29, 14, 0, 0, 0, time.UTC)

func produceClaim(swapMode protocol.SwapMode) *processes.NodeClaim {
	return &processes.NodeClaim{
		Role:                protocol.ClaimRoleProduce,
		SwapMode:            swapMode,
		PayloadCode:         "WIDGET-A",
		CoreNodeName:        "PRODUCE-NODE",
		InboundSource:       "EMPTY-STORAGE",
		InboundStaging:      "PRODUCE-IN-STAGING",
		OutboundStaging:     "PRODUCE-OUT-STAGING",
		OutboundDestination: "FILLED-STORAGE",
		PairedCoreNode:      "PRODUCE-NODE-BACK",
	}
}

func produceFixtures(swapMode protocol.SwapMode) (*processes.Node, *processes.RuntimeState, *processes.NodeClaim) {
	node := &processes.Node{ID: 1, Name: "PRODUCE-NODE"}
	runtime := &processes.RuntimeState{RemainingUOPCached: 50}
	return node, runtime, produceClaim(swapMode)
}

// testManifest is the resolved template a produce plan is handed. Production
// resolves it from Core (producedManifest); these tests are about the plan's
// SHAPE, so one line whose part number is visibly not the payload code is
// enough — and it fails loudly if anything starts deriving the manifest from
// the claim again.
func testManifest() []protocol.IngestManifestItem {
	return []protocol.IngestManifestItem{{PartNumber: "PART-FROM-TEMPLATE", Quantity: 1}}
}

func TestBuildProducePlan_NoSwapModeErrors(t *testing.T) {
	t.Parallel()
	node, runtime, claim := produceFixtures("")

	// Produce with no swap mode (retired simple-produce) must fail loud.
	if _, err := BuildProducePlan(node, runtime, claim, fixedNow, nil, nil, testManifest()); err == nil {
		t.Fatal("BuildProducePlan: want error for a produce claim with no swap mode")
	}
}

func TestBuildProducePlan_Sequential(t *testing.T) {
	t.Parallel()
	node, runtime, claim := produceFixtures("sequential")

	plan, err := BuildProducePlan(node, runtime, claim, fixedNow, nil, nil, testManifest())
	if err != nil {
		t.Fatalf("BuildProducePlan: %v", err)
	}
	if plan.Dispatch == nil {
		t.Fatalf("sequential must have a Dispatch")
	}
	if !plan.Dispatch.AutoConfirmA {
		t.Errorf("sequential's removal order is dispatched via the auto-confirm path; AutoConfirmA = false, want true")
	}
	if plan.Dispatch.StepsB != nil {
		t.Errorf("sequential is single-order; StepsB should be nil")
	}
	if plan.Dispatch.RequiresActiveSwapGuard {
		t.Errorf("sequential should not require swap guard (backfill is auto-created on transit)")
	}
}

func TestBuildProducePlan_TwoRobotPressIndex_OK(t *testing.T) {
	t.Parallel()
	node, runtime, claim := produceFixtures("two_robot_press_index")

	plan, err := BuildProducePlan(node, runtime, claim, fixedNow, nil, nil, testManifest())
	if err != nil {
		t.Fatalf("BuildProducePlan: %v", err)
	}
	if plan.Dispatch == nil || plan.Dispatch.StepsA == nil || plan.Dispatch.StepsB == nil {
		t.Errorf("two_robot_press_index must produce both R1 and R2 steps via Dispatch")
	}
	if plan.Dispatch != nil && !plan.Dispatch.RequiresActiveSwapGuard {
		t.Errorf("two_robot_press_index must require swap guard")
	}
}

func TestBuildProducePlan_PreconditionErrors(t *testing.T) {
	t.Parallel()
	node, runtime, claim := produceFixtures("")

	t.Run("nil_claim", func(t *testing.T) {
		if _, err := BuildProducePlan(node, runtime, nil, fixedNow, nil, nil, testManifest()); err == nil {
			t.Fatalf("expected error for nil claim")
		}
	})

	t.Run("wrong_role", func(t *testing.T) {
		c := *claim
		c.Role = protocol.ClaimRoleConsume
		if _, err := BuildProducePlan(node, runtime, &c, fixedNow, nil, nil, testManifest()); err == nil {
			t.Fatalf("expected error for non-produce role")
		}
	})

	t.Run("zero_uop", func(t *testing.T) {
		r := *runtime
		r.RemainingUOPCached = 0
		if _, err := BuildProducePlan(node, &r, claim, fixedNow, nil, nil, testManifest()); err == nil {
			t.Fatalf("expected error for zero RemainingUOP")
		}
	})
}

// TestProducedManifest_CarriesTheTemplatesPartNumbers pins what a produced
// carrier says it contains.
//
// Both produce sites used to stamp claim.PayloadCode as the manifest line. A
// payload code names a KIND OF CONTENT and a manifest line names a PART, so the
// two coincide for a bin of one part and diverge for a kit — and a plant whose
// lines do not spell the part the way its payload codes do diverges on every
// bin. Where they diverge, every produced carrier was uncountable: material.go
// found no parts_per_cycle for it, built no rows, and the part reached the
// inventory ledger nowhere at all.
//
// One line per template line, because a carrier can hold several parts and the
// path already handles it: a transaction per part, EntryNumber rolled over them
// under one ticket. Asking the template is what makes the kit and the
// single-part bin one path — nothing at the produce site can tell them apart.
func TestProducedManifest_CarriesTheTemplatesPartNumbers(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(PayloadManifestResponse{
			UOPCapacity: 20,
			Items: []ManifestItem{
				{PartNumber: "51015-LH", PartsPerCycle: 1, Description: "left bracket"},
				{PartNumber: "51015-RH", PartsPerCycle: 2, Description: "right bracket"},
			},
		})
	}))
	defer srv.Close()

	eng := &Engine{coreClient: NewCoreClient(srv.URL), logFn: func(string, ...any) {}}
	got := eng.producedManifest("7332B4-6RR0A.06", 12)

	if len(got) != 2 {
		t.Fatalf("built %d lines, want 2 — a carrier holding two parts books two "+
			"transactions, and the wire numbers them 1 and 2 under one ticket", len(got))
	}
	for i, want := range []string{"51015-LH", "51015-RH"} {
		if got[i].PartNumber != want {
			t.Errorf("line %d part number = %q, want %q. %q is the PAYLOAD CODE — the "+
				"name of the kit, not of either part in it; the ledger looks "+
				"parts_per_cycle up per part and finds nothing for the kit's name.",
				i, got[i].PartNumber, want, "7332B4-6RR0A.06")
		}
		if got[i].Quantity != 12 {
			t.Errorf("line %d quantity = %d, want the cycle count 12", i, got[i].Quantity)
		}
	}
}

// TestProducedManifest_NoTemplateFallsBackAndSaysSo: a payload Core has no
// template for still produces a carrier, stamped with the payload code exactly
// as before. For a single-part payload that is the right answer and for a kit it
// is a fiction, and nothing here can tell which — so it is deliberately not
// papered over: inventing a part number nobody supplied would be worse than the
// loud line and the uncounted-lines report that follow.
func TestProducedManifest_NoTemplateFallsBackAndSaysSo(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(PayloadManifestResponse{UOPCapacity: 10})
	}))
	defer srv.Close()

	var logged []string
	eng := &Engine{
		coreClient: NewCoreClient(srv.URL),
		logFn:      func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) },
	}
	got := eng.producedManifest("UNTEMPLATED", 5)

	if len(got) != 1 || got[0].PartNumber != "UNTEMPLATED" {
		t.Fatalf("fallback = %+v, want one line carrying the payload code", got)
	}
	var said bool
	for _, l := range logged {
		if strings.Contains(l, "UNTEMPLATED") && strings.Contains(l, "template") {
			said = true
		}
	}
	if !said {
		t.Errorf("nothing was logged. A carrier the ledger cannot count must not leave "+
			"quietly.\nlogs: %v", logged)
	}
}
