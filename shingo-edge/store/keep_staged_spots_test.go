package store

import (
	"errors"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// keep_staged_spots_test.go — the dedicated-spot check at the store's doors
// (processes.CheckKeepStagedSpots). The composer's door, where a spot moves
// between cells in one save, is pinned in engine/.

// spotClaim is a two_robot claim at line, staging at staging, keeping a spare
// there when keep is set (the same node as both).
func spotClaim(styleID int64, line, staging string, keep bool) processes.NodeClaimInput {
	spot := ""
	if keep {
		spot = staging
	}
	return processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: line, Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeTwoRobot,
		PayloadCode: "PART", InboundStaging: staging, InboundSource: "MARKET", OutboundDestination: "DEST",
		KeepStagedNode: domain.Ptr(spot),
	}
}

func wantSpotRefusal(t *testing.T, err error, mustName ...string) {
	t.Helper()
	if !errors.Is(err, domain.ErrKeepStagedSpot) {
		t.Fatalf("err = %v, want the dedicated-spot refusal", err)
	}
	for _, s := range mustName {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("refusal does not name %q: %v", s, err)
		}
	}
}

// TestKeepStagedSpot_RefusedInBothSaveOrders: whichever claim is saved second
// meets the check — a claim naming a node that is already a kept spot, and a
// claim naming as its keep-staged node a node another claim already names.
func TestKeepStagedSpot_RefusedInBothSaveOrders(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	_, sid := seedProcessStyle(t, db, "KS-PROC", "KS-STYLE")

	// (a) The spot exists first; a second claim names it as its destination.
	if _, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, spotClaim(sid, "LINE-A", "SPOT-A", true)); err != nil {
		t.Fatalf("keep-staged claim: %v", err)
	}
	later := spotClaim(sid, "LINE-B", "STG-B", false)
	later.OutboundDestination = "SPOT-A"
	_, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, later)
	wantSpotRefusal(t, err, "SPOT-A", "LINE-A", "LINE-B", "outbound_destination", "this Edge's claims")

	// (b) Two claims share plain staging, which is allowed; naming it as one's
	// keep-staged node makes the other's staging a touch on its spot.
	if _, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, spotClaim(sid, "LINE-C", "SHARED", false)); err != nil {
		t.Fatalf("first sharer: %v", err)
	}
	if _, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, spotClaim(sid, "LINE-D", "SHARED", false)); err != nil {
		t.Fatalf("shared staging without a kept spare is refused: %v", err)
	}
	_, err = db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, spotClaim(sid, "LINE-D", "SHARED", true))
	wantSpotRefusal(t, err, "SHARED", "LINE-C", "LINE-D", "inbound_staging")
	c, err := db.GetStyleNodeClaimByNode(sid, "LINE-D")
	if c = testutil.Must(t, c, err, "read LINE-D"); c == nil || c.KeepStagedNode != "" {
		t.Errorf("the refused write landed: %+v", c)
	}
}

// TestKeepStagedSpot_LinePositionAcrossStylesIsRefused: another style of the
// same process may reuse a kept spot as staging (the clone pin), but not as a
// line position.
func TestKeepStagedSpot_LinePositionAcrossStylesIsRefused(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	pid, s1 := seedProcessStyle(t, db, "KS-PROC", "KS-S1")
	s2, err := db.CreateStyle("KS-S2", "", pid)
	if err != nil {
		t.Fatalf("second style: %v", err)
	}
	if _, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, spotClaim(s1, "LINE-1", "SPOT-L", true)); err != nil {
		t.Fatalf("keep-staged claim: %v", err)
	}
	_, err = db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, spotClaim(s2, "SPOT-L", "STG-2", false))
	wantSpotRefusal(t, err, "SPOT-L", "core_node_name", "same process")
}

// TestKeepStagedSpot_CopyIntoAnotherProcessIsRefused: a copy carries
// keep_staged_node, so copying a kept spare into a style of another process would
// put one spot on two processes' lines. The whole copy rolls back.
func TestKeepStagedSpot_CopyIntoAnotherProcessIsRefused(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	_, src := seedProcessStyle(t, db, "KS-P1", "KS-SRC")
	_, dst := seedProcessStyle(t, db, "KS-P2", "KS-DST")
	if _, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, spotClaim(src, "LINE-1", "SPOT-C", true)); err != nil {
		t.Fatalf("keep-staged claim: %v", err)
	}
	if _, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, spotClaim(dst, "LINE-9", "STG-9", false)); err != nil {
		t.Fatalf("target claim: %v", err)
	}
	_, err := db.CopyStyleClaims(domain.CoreNodeKinds{}, src, dst, true, nil)
	wantSpotRefusal(t, err, "SPOT-C", "another process")
	claims, err := db.ListStyleNodeClaims(dst)
	if err != nil {
		t.Fatalf("list target: %v", err)
	}
	if len(claims) != 1 || claims[0].CoreNodeName != "LINE-9" {
		t.Errorf("a refused copy changed the target: %+v", claims)
	}
}

// TestKeepStagedSpot_MovingABusySpotIsRefused: an order still delivering to
// the spot holds it in place; once that order is terminal the move lands.
func TestKeepStagedSpot_MovingABusySpotIsRefused(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	_, sid := seedProcessStyle(t, db, "KS-PROC", "KS-STYLE")
	if _, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, spotClaim(sid, "LINE-M", "SPOT-OLD", true)); err != nil {
		t.Fatalf("keep-staged claim: %v", err)
	}
	orderID, err := db.CreateOrder("ks-refill", protocol.OrderTypeRetrieve, nil, false, 1,
		"SPOT-OLD", "", "MARKET", "", true, "PART", "", "")
	if err != nil {
		t.Fatalf("refill order: %v", err)
	}

	_, err = db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, spotClaim(sid, "LINE-M", "SPOT-NEW", true))
	wantSpotRefusal(t, err, "SPOT-OLD", "1 open order")

	if _, err := db.Exec(`UPDATE orders SET status = ? WHERE id = ?`, string(protocol.StatusCancelled), orderID); err != nil {
		t.Fatalf("finish order: %v", err)
	}
	if _, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, spotClaim(sid, "LINE-M", "SPOT-NEW", true)); err != nil {
		t.Fatalf("move with no open order refused: %v", err)
	}
}

// TestKeepStagedSpot_ANodeOfItsOwn: the keep-staged node need not be the
// claim's inbound staging. A spot of its own saves; the claim's own routes may
// not name it, other than its inbound staging; and a second claim in the style
// may not keep a spare on it too.
func TestKeepStagedSpot_ANodeOfItsOwn(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	_, sid := seedProcessStyle(t, db, "KS-PROC", "KS-STYLE")

	own := spotClaim(sid, "LINE-O", "STG-O", false)
	own.KeepStagedNode = domain.Ptr("SPOT-O")
	if _, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, own); err != nil {
		t.Fatalf("a spot of its own beside the inbound staging: %v", err)
	}

	selfTouch := spotClaim(sid, "LINE-S", "STG-S", false)
	selfTouch.KeepStagedNode, selfTouch.OutboundDestination = domain.Ptr("SPOT-S"), "SPOT-S"
	_, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, selfTouch)
	wantSpotRefusal(t, err, "SPOT-S", "LINE-S", "outbound_destination")

	second := spotClaim(sid, "LINE-T", "STG-T", false)
	second.KeepStagedNode = domain.Ptr("SPOT-O")
	_, err = db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, second)
	wantSpotRefusal(t, err, "SPOT-O", "LINE-O", "LINE-T", "keep_staged_node")
}

// TestKeepStagedSpot_ALaneIsRefused: the keep-staged node is a claim leg, so a
// lane on it is refused at the store's door like a lane on any other leg.
func TestKeepStagedSpot_ALaneIsRefused(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	_, sid := seedProcessStyle(t, db, "KS-PROC", "KS-STYLE")
	in := spotClaim(sid, "LINE-L", "STG-L", false)
	in.KeepStagedNode = domain.Ptr("KS-LANE")
	_, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{Lanes: map[string]bool{"KS-LANE": true}}, in)
	if !errors.Is(err, domain.ErrLaneLeg) || !strings.Contains(err.Error(), "keep_staged_node") {
		t.Fatalf("err = %v, want the lane refusal on keep_staged_node", err)
	}
}
