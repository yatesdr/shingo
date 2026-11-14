package service

import (
	"context"
	"maps"
	"testing"

	"shingo/protocol"
	"shingoedge/internal/testdb"
	"shingoedge/store/stations"
)

// TestRobotWatchOrders_TheBoardsTrackedOrdersWithAUUID pins what the live
// robot feed watches for one operator station: the board's orders (its tiles'
// own, and an order of another station's tile that sources from one of its
// tiles) whose status Core sends a position for, uuid → Edge order id. A
// queued, delivered, reshuffling or ended order is not watched, nor another
// station's own. Held against BuildView's tiles, so the two cannot drift, and
// counted: four statements with no changeover.
func TestRobotWatchOrders_TheBoardsTrackedOrdersWithAUUID(t *testing.T) {
	t.Parallel()
	db, counter := testdb.OpenCounting(t)
	svc := NewStationService(db)
	pid, err := db.CreateProcess("P", "", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	n, _ := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "N", Enabled: true})
	m, _ := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "M", Enabled: true})
	if err := setNodesErr(svc, n, []string{"LN01", "LN02"}); err != nil {
		t.Fatalf("set N's nodes: %v", err)
	}
	if err := setNodesErr(svc, m, []string{"LM01"}); err != nil {
		t.Fatalf("set M's nodes: %v", err)
	}
	nodeID := map[string]int64{}
	for _, st := range []int64{n, m} {
		nodes, err := db.ListProcessNodesByStation(st)
		if err != nil {
			t.Fatalf("list nodes: %v", err)
		}
		for _, node := range nodes {
			nodeID[node.CoreNodeName] = node.ID
		}
	}
	order := func(uuid, node, source string, status protocol.Status) int64 {
		t.Helper()
		pn := nodeID[node]
		id, err := db.CreateOrder(uuid, protocol.OrderTypeRetrieve, &pn, false, 1, node, "", source, protocol.Height{}, false, "", "", "")
		if err != nil {
			t.Fatalf("create order %s: %v", uuid, err)
		}
		if err := db.UpdateOrderStatus(id, string(status)); err != nil {
			t.Fatalf("status of %s: %v", uuid, err)
		}
		return id
	}
	wantN := map[string]int64{
		"u-dispatched": order("u-dispatched", "LN01", "", protocol.StatusDispatched),
		"u-in-transit": order("u-in-transit", "LN02", "", protocol.StatusInTransit),
		"u-staged":     order("u-staged", "LN01", "", protocol.StatusStaged),
		"u-faulted":    order("u-faulted", "LN02", "", protocol.StatusFaulted),
	}
	order("u-queued", "LN01", "", protocol.StatusQueued)
	order("u-delivered", "LN01", "", protocol.StatusDelivered)
	order("u-reshuffling", "LN02", "", protocol.StatusReshuffling)
	order("u-confirmed", "LN02", "", protocol.StatusConfirmed)
	wantM := map[string]int64{"u-m": order("u-m", "LM01", "", protocol.StatusInTransit)}
	sourced := order("u-sourced", "LM01", "LN02", protocol.StatusInTransit)
	wantN["u-sourced"], wantM["u-sourced"] = sourced, sourced

	counter.Reset()
	got, err := svc.RobotWatchOrders(n)
	statements := counter.Count()
	if err != nil {
		t.Fatalf("RobotWatchOrders(N): %v", err)
	}
	if !maps.Equal(got, wantN) {
		t.Errorf("N watches %v, want %v", got, wantN)
	}
	if statements != 4 {
		t.Errorf("N's watch cost %d statements, want 4: the station, its changeover, its tiles and one order read", statements)
	}
	if gotM, err := svc.RobotWatchOrders(m); err != nil || !maps.Equal(gotM, wantM) {
		t.Errorf("M watches %v (%v), want %v", gotM, err, wantM)
	}

	view, err := svc.BuildView(context.Background(), n)
	if err != nil {
		t.Fatalf("BuildView(N): %v", err)
	}
	board := map[string]int64{}
	for _, tile := range view.Nodes {
		for _, o := range tile.Orders {
			if protocol.IsVendorTracked(o.Status) {
				board[o.UUID] = o.ID
			}
		}
	}
	if !maps.Equal(board, got) {
		t.Errorf("N's board shows the tracked orders %v and the feed watches %v; they must be one set", board, got)
	}
}
