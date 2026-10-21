package service

import (
	"encoding/json"
	"slices"
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// station_bins_test.go pins the board's bin list (BinNodes: what it holds,
// and that it is read when the plant moves and not per build) and the stamp
// (StampBins: the buffer strip, a home's buffer partial, the empties).

// loaderSet resolves a node to whichever of its loaders contains it.
type loaderSet []*domain.Loader

func (s loaderSet) LoaderForNode(n domain.NodeID) (*domain.Loader, error) {
	for _, l := range s {
		if l.Contains(n) {
			return l, nil
		}
	}
	return nil, nil
}

func mustLoader(t *testing.T) func(*domain.Loader, error) *domain.Loader {
	return func(l *domain.Loader, err error) *domain.Loader {
		t.Helper()
		if err != nil {
			t.Fatalf("build loader: %v", err)
		}
		return l
	}
}

// binsPlant is a dedicated loader (homes H1, H2 on station A; buffer spots
// F1, F2 on no station), an unloader (window U1 on station B, outbound group
// GRP of M1 and M2), a stage-1 unloader (window S1 on station B, outbound
// group GRP2 of X1) and a stationless position P2 of station B's process.
func binsPlant(t *testing.T, db *store.DB) (*StationService, int64, int64) {
	t.Helper()
	homes := mustLoader(t)(domain.NewDedicatedPositionsLoader("homes", domain.RoleProduce, domain.ReplenishmentOperator,
		[]domain.Position{
			{Node: "H1", Payload: "PART-A", HomeKind: domain.HomeKindHome},
			{Node: "H2", Payload: "PART-B", HomeKind: domain.HomeKindHome},
			{Node: "F1", HomeKind: domain.HomeKindBuffer},
			{Node: "F2", HomeKind: domain.HomeKindBuffer},
		}))
	unl := mustLoader(t)(domain.NewSharedWindowLoader("unl", domain.RoleConsume, domain.ReplenishmentOperator,
		[]domain.Window{{Node: "U1"}}, nil, domain.WithOutboundDest("GRP")))
	stage1 := mustLoader(t)(domain.NewSharedWindowLoader("stage1", domain.RoleConsume, domain.ReplenishmentOperator,
		[]domain.Window{{Node: "S1"}}, nil, domain.WithOutboundDest("GRP2"), domain.WithLeavesBare(true)))
	svc := NewStationService(db)
	svc.SetLoaderResolver(loaderSet{homes, unl, stage1})
	svc.SetCoreNodeGroupResolver(func() map[string][]string {
		return map[string][]string{"GRP": {"M1", "M2"}, "GRP2": {"X1"}}
	})
	stationWith := func(code string, nodes ...string) (int64, int64) {
		pid, err := db.CreateProcess(code+"-PROC", "", "", "", false)
		if err != nil {
			t.Fatalf("create process: %v", err)
		}
		sid, err := db.CreateOperatorStation(stations.Input{ProcessID: pid, Code: code, Name: code, Sequence: 1, Enabled: true})
		if err != nil {
			t.Fatalf("create station: %v", err)
		}
		for i, n := range nodes {
			if _, err := db.CreateProcessNode(processes.NodeInput{ProcessID: pid, OperatorStationID: &sid,
				CoreNodeName: n, Code: n, Name: n, Sequence: i + 1, Enabled: true}); err != nil {
				t.Fatalf("create node %s: %v", n, err)
			}
		}
		return pid, sid
	}
	_, a := stationWith("A", "H1", "H2")
	bProc, b := stationWith("B", "U1", "S1")
	if _, err := db.CreateProcessNode(processes.NodeInput{ProcessID: bProc, CoreNodeName: "P2", Code: "P2", Name: "P2",
		Sequence: 9, Enabled: true}); err != nil {
		t.Fatalf("create stationless node: %v", err)
	}
	return svc, a, b
}

// TestBinNodes_TheBoardsWholeList: a board's list is its tiles, the
// stationless positions of its process, its dedicated loaders' buffer spots
// and its consume loaders' outbound-group members, sorted, each once. A
// stage-1 unloader's group (stage 2's windows) is not listed.
func TestBinNodes_TheBoardsWholeList(t *testing.T) {
	db, _ := countingDB(t)
	svc, a, b := binsPlant(t, db)
	if got, want := svc.BinNodes(a), []string{"F1", "F2", "H1", "H2"}; !slices.Equal(got, want) {
		t.Errorf("station A list = %v, want %v", got, want)
	}
	if got, want := svc.BinNodes(b), []string{"M1", "M2", "P2", "S1", "U1"}; !slices.Equal(got, want) {
		t.Errorf("station B list = %v, want %v", got, want)
	}
}

// TestBinNodes_ReadWhenThePlantMovesNotPerBuild: the list costs one statement
// the first time and none after, until process_nodes is written, the node
// list replaces the groups, or the loaders are replaced.
func TestBinNodes_ReadWhenThePlantMovesNotPerBuild(t *testing.T) {
	db, counter := countingDB(t)
	svc, a, _ := binsPlant(t, db)
	var plant, loaders uint64
	svc.SetPlantGenerationResolver(func() uint64 { return plant })
	svc.SetLoaderGenerationResolver(func() uint64 { return loaders })
	cost := func() int64 {
		counter.Reset()
		svc.BinNodes(a)
		return counter.Count()
	}
	if n := cost(); n != 1 {
		t.Errorf("first read = %d statements, want 1", n)
	}
	if n := cost(); n != 0 {
		t.Errorf("steady read = %d statements, want 0", n)
	}
	plant++
	if n := cost(); n != 1 {
		t.Errorf("after the node list moved = %d statements, want 1", n)
	}
	loaders++
	if n := cost(); n != 1 {
		t.Errorf("after the loaders moved = %d statements, want 1", n)
	}
	processes.BumpNodeGeneration()
	if n := cost(); n != 1 {
		t.Errorf("after process_nodes moved = %d statements, want 1", n)
	}
}

func stampJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// homesView is station A's view as BuildView leaves it: two dedicated tiles.
func homesView(orders ...domain.Order) *domain.OperatorStationView {
	tile := func(node string) domain.StationNodeView {
		return domain.StationNodeView{Node: domain.Node{CoreNodeName: node},
			Loader: &domain.TileLoader{Role: "produce", Layout: "dedicated_positions"}, Orders: []domain.Order{}}
	}
	v := &domain.OperatorStationView{Nodes: []domain.StationNodeView{tile("H1"), tile("H2")}}
	v.Nodes[0].Orders = append(v.Nodes[0].Orders, orders...)
	return v
}

// TestStampBins_BufferStrip: each spot's state from its row, in the plant's
// order, on every tile of the loader; a home's clear in flight makes its spot
// an empty; a bins read that failed leaves every spot unknown and stamps no
// bin and no partial.
func TestStampBins_BufferStrip(t *testing.T) {
	db, _ := countingDB(t)
	svc, _, _ := binsPlant(t, db)
	home := protocol.NodeBinInfo{NodeName: "H1", Occupied: true, PayloadCode: "PART-A", UOPRemaining: 30}
	cases := []struct {
		name       string
		f1, f2     *protocol.NodeBinInfo
		orders     []domain.Order
		read       bool
		wantStrip  string
		wantPartly string
	}{
		{name: "a partial of the home's part and an open spot",
			f1:        &protocol.NodeBinInfo{Occupied: true, PayloadCode: "PART-A", UOPRemaining: 4},
			read:      true,
			wantStrip: `[{"node":"F1","state":"partial"},{"node":"F2","state":"open"}]`, wantPartly: `{"node":"F1"}`},
		{name: "an emptied bin, and a part at a count of 0",
			f1:        &protocol.NodeBinInfo{Occupied: true},
			f2:        &protocol.NodeBinInfo{Occupied: true, PayloadCode: "PART-A"},
			read:      true,
			wantStrip: `[{"node":"F1","state":"empty"},{"node":"F2","state":"unknown"}]`, wantPartly: `null`},
		{name: "a count with no part, and a partial of another part",
			f1:        &protocol.NodeBinInfo{Occupied: true, UOPRemaining: 3},
			f2:        &protocol.NodeBinInfo{Occupied: true, PayloadCode: "PART-B", UOPRemaining: 3},
			read:      true,
			wantStrip: `[{"node":"F1","state":"unknown"},{"node":"F2","state":"partial"}]`, wantPartly: `null`},
		{name: "the home's clear in flight to F2",
			f2:   &protocol.NodeBinInfo{Occupied: true, PayloadCode: "PART-A", UOPRemaining: 4},
			read: true,
			orders: []domain.Order{{ID: 1, OrderType: protocol.OrderTypeMove, Status: protocol.StatusQueued,
				SourceNode: "H1", DeliveryNode: "F2"}},
			wantStrip: `[{"node":"F1","state":"open"},{"node":"F2","state":"empty"}]`, wantPartly: `{"node":"F2"}`},
		{name: "the bins were not read",
			read:      false,
			wantStrip: `[{"node":"F1","state":"unknown"},{"node":"F2","state":"unknown"}]`, wantPartly: `null`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows := map[string]protocol.NodeBinInfo{"H1": home, "H2": {NodeName: "H2"}}
			for name, r := range map[string]*protocol.NodeBinInfo{"F1": tc.f1, "F2": tc.f2} {
				row := protocol.NodeBinInfo{NodeName: name}
				if r != nil {
					row = *r
					row.NodeName = name
				}
				rows[name] = row
			}
			if !tc.read {
				rows = nil
			}
			v := homesView(tc.orders...)
			svc.StampBins(v, httpBins(rows), tc.read)
			if got := stampJSON(t, v.Nodes[0].Loader.Buffer); got != tc.wantStrip {
				t.Errorf("H1 strip = %s, want %s", got, tc.wantStrip)
			}
			if got := stampJSON(t, v.Nodes[0].BufferPartial); got != tc.wantPartly {
				t.Errorf("H1 buffer_partial = %s, want %s", got, tc.wantPartly)
			}
			if v.Nodes[1].BufferPartial != nil {
				t.Errorf("H2 (no bin) buffer_partial = %+v", v.Nodes[1].BufferPartial)
			}
			if !tc.read && v.Nodes[0].BinState != nil {
				t.Errorf("not read, yet H1 has a bin: %+v", v.Nodes[0].BinState)
			}
		})
	}
}

// TestStampBins_OneTileOfALoaderCarriesItsStrip: a dedicated loader's buffer
// strip rides once, on its first home in the plant's order that is a tile of
// the board, and on no other tile; with that home off the board, the next.
func TestStampBins_OneTileOfALoaderCarriesItsStrip(t *testing.T) {
	db, _ := countingDB(t)
	svc, _, _ := binsPlant(t, db)
	carriers := func(v *domain.OperatorStationView) []string {
		var out []string
		for _, tile := range v.Nodes {
			if tile.Loader.Buffer != nil {
				out = append(out, tile.Node.CoreNodeName)
			}
		}
		return out
	}
	v := homesView()
	svc.StampBins(v, nil, false)
	if got := carriers(v); !slices.Equal(got, []string{"H1"}) {
		t.Errorf("tiles carrying the strip = %v, want only H1", got)
	}
	v = homesView()
	v.Nodes = v.Nodes[1:]
	svc.StampBins(v, nil, false)
	if got := carriers(v); !slices.Equal(got, []string{"H2"}) {
		t.Errorf("with H1 off the board, tiles carrying the strip = %v, want only H2", got)
	}
}

// TestStampBins_Empties: the unloader board's empties are counted from its
// outbound group's members: an occupied bin with no part and no count is an
// empty of its type; a bare cart and a full are not; an unoccupied member is
// an open spot. A bin type a served press fills gets its line with none.
func TestStampBins_Empties(t *testing.T) {
	db, _ := countingDB(t)
	svc, _, _ := binsPlant(t, db)
	rows := map[string]protocol.NodeBinInfo{
		"U1": {NodeName: "U1", Occupied: true, PayloadCode: "PART-C", UOPRemaining: 40, BinTypeCode: "TOTE-1"},
		"M1": {NodeName: "M1", Occupied: true, BinTypeCode: "TOTE-1"},
		"M2": {NodeName: "M2"},
	}
	view := func() *domain.OperatorStationView {
		return &domain.OperatorStationView{
			Nodes: []domain.StationNodeView{{Node: domain.Node{CoreNodeName: "U1"},
				Loader: &domain.TileLoader{Role: "consume", Layout: "shared_window", OutboundDest: "GRP"}}},
			Serves: []domain.ServedRow{{Cell: "PRESS-1", Part: "PART-C", Role: "produce", Carrier: "KD-1"}},
		}
	}
	v := view()
	svc.StampBins(v, httpBins(rows), true)
	want := `{"group":"GRP","rows":[{"bin_type":"KD-1","have":0,"spots":1},{"bin_type":"TOTE-1","have":1,"spots":1}]}`
	if got := stampJSON(t, v.Empties); got != want {
		t.Errorf("empties = %s, want %s", got, want)
	}

	rows["M1"] = protocol.NodeBinInfo{NodeName: "M1", Occupied: true, BinTypeCode: "TOTE-1", Bare: true}
	rows["M2"] = protocol.NodeBinInfo{NodeName: "M2", Occupied: true, BinTypeCode: "TOTE-1", PayloadCode: "PART-C", UOPRemaining: 5}
	v = view()
	svc.StampBins(v, httpBins(rows), true)
	want = `{"group":"GRP","rows":[{"bin_type":"KD-1","have":0,"spots":0}]}`
	if got := stampJSON(t, v.Empties); got != want {
		t.Errorf("a bare cart and a full = %s, want %s", got, want)
	}

	v = view()
	svc.StampBins(v, nil, false)
	if v.Empties != nil {
		t.Errorf("not read, yet empties = %s", stampJSON(t, v.Empties))
	}
}

// httpBins is a node-bins answer as the board reads it over HTTP.
func httpBins(rows map[string]protocol.NodeBinInfo) map[string]BoardBin {
	infos := make([]protocol.NodeBinInfo, 0, len(rows))
	for _, r := range rows {
		infos = append(infos, r)
	}
	return BoardBinsFromHTTP(infos)
}

// TestNodeBinState_HeldPartialFromEitherSource: a held partial reaches the
// tile's bin the same from both sources: the part's capacity, its line, its
// arrival and its count. Over HTTP the count is the row's own; the copy carries
// no count but a held partial's (held_uop), which does not tick. A carrier that
// is not held keeps no count from the copy.
func TestNodeBinState_HeldPartialFromEitherSource(t *testing.T) {
	t.Parallel()
	const since = "2026-10-18T10:00:00Z"
	viaHTTP := httpBins(map[string]protocol.NodeBinInfo{"HOME-1": {NodeName: "HOME-1", Occupied: true, BinID: 7,
		PayloadCode: "PART-H", UOPRemaining: 40, UOPCapacity: 100, HeldSince: since, HeldFor: "LINE-1"}})["HOME-1"]
	viaCopy := BoardBinsFromCopy(map[string]protocol.NodeBinsRow{"HOME-1": {Node: "HOME-1", Occupied: true, BinID: 7,
		PayloadCode: "PART-H", Stocked: true, UOPCapacity: 100, HeldSince: since, HeldFor: "LINE-1", HeldUOP: 40}})["HOME-1"]
	for name, row := range map[string]BoardBin{"http": viaHTTP, "copy": viaCopy} {
		got := nodeBinState(row)
		if got.UOPCapacity != 100 || got.HeldSince != since || got.HeldFor != "LINE-1" || got.UOPRemaining != 40 {
			t.Errorf("%s: bin = capacity %d, held since %q for %q, count %d; want 100, %q, LINE-1, 40",
				name, got.UOPCapacity, got.HeldSince, got.HeldFor, got.UOPRemaining, since)
		}
	}
	notHeld := nodeBinState(BoardBinsFromCopy(map[string]protocol.NodeBinsRow{"L-1": {Node: "L-1", Occupied: true,
		PayloadCode: "PART-H", Stocked: true, UOPCapacity: 100}})["L-1"])
	if notHeld.UOPRemaining != 0 || notHeld.HeldSince != "" || notHeld.UOPCapacity != 100 {
		t.Errorf("not held, from the copy: count %d, held since %q, capacity %d; want 0, none, 100",
			notHeld.UOPRemaining, notHeld.HeldSince, notHeld.UOPCapacity)
	}
}
