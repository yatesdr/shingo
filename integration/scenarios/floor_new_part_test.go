// A part made new at a station's changeover picker, end to end across the two
// modules: the Edge submits it, Core makes it and answers, the Edge applies the
// answer, the station mints a style with it from the cell's recipe and starts
// the changeover to that style. Then the same submit again, answered as a
// repeat with nothing new written.
//
// Core is the real engine behind its real ask server (testharness.NewCore), with the
// catalog read the part.create door answers with and the memo drop after a
// payload write wired as cmd/shingocore wires them. The ask and its answer
// travel the Bus.
//
//go:build docker

package scenarios

import (
	"encoding/json"
	"testing"
	"time"

	"shingo/integration/harness"
	"shingo/protocol"

	coreservice "shingocore/service"
	corestore "shingocore/store"
	corebins "shingocore/store/bins"
	corenodes "shingocore/store/nodes"
	coreorders "shingocore/store/orders"
	corepayloads "shingocore/store/payloads"
	coreharness "shingocore/testharness"

	"shingoedge/domain"
	edgeengine "shingoedge/engine"
	edgeservice "shingoedge/service"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
	edgeharness "shingoedge/testharness"
)

func TestScenario_FloorNewPart_SubmitMintStartChangeover(t *testing.T) {
	t.Parallel()
	core := startKeepStagedCore(t)
	coreDB := core.eng.DB()
	const (
		line, spot, empties, dest = "SYN-FNP-LINE", "SYN-FNP-SPOT", "SYN-FNP-EMPTIES", "SYN-FNP-DEST"
		running, newPart, cart    = "SYN-FNP-RUN", "SYN-FNP-NEW", "SYN-FNP-CART"
		edgeStation, operator     = "edge.test", "SYN-FNP Station"
	)

	// ── Core: the cell's nodes, the carrier type and the part it runs ──
	at := map[string]*corenodes.Node{}
	for _, name := range []string{line, spot, empties, dest} {
		at[name] = &corenodes.Node{Name: name, Enabled: true}
		mustNil(t, coreDB.CreateNode(at[name]), "node "+name)
	}
	carrier := &corebins.BinType{Code: cart}
	mustNil(t, coreDB.CreateBinType(carrier), "carrier type")
	runningPart := &corepayloads.Payload{Code: running, UOPCapacity: 40}
	mustNil(t, coreDB.CreatePayload(runningPart), "running part")
	mustNil(t, coreDB.SetPayloadBinTypes(runningPart.ID, []int64{carrier.ID}), "running part's carrier")
	lineBin := &corebins.Bin{BinTypeID: carrier.ID, Label: "SYN-FNP-LINE-BIN", NodeID: &at[line].ID, Status: "staged"}
	mustNil(t, coreDB.CreateBin(lineBin), "the line's bin")
	mustNil(t, coreDB.SetBinManifest(lineBin.ID, `{"items":[]}`, running, 30), "the line's manifest")
	mustNil(t, coreDB.ConfirmBinManifest(lineBin.ID, ""), "the line's confirm")
	empty := &corebins.Bin{BinTypeID: carrier.ID, Label: "SYN-FNP-EMPTY", NodeID: &at[empties].ID, Status: "available"}
	mustNil(t, coreDB.CreateBin(empty), "an empty carrier at the source")
	// Core's tables and hooks as main builds them, less what this cell leaves
	// out: every subject but the asks (its Edge also sends a demand origin,
	// never routed here), the release, the redirect and the ingest, and every
	// hook but the change door and the catalog row the floor part answers with.
	coreSide := coreharness.NewCore(t, core.eng,
		coreharness.RoutesOnly(protocol.TypeOrderRequest, protocol.TypeComplexOrderRequest, protocol.TypeOrderCancel,
			protocol.TypeOrderReceipt, protocol.SubjectEdgeAsk),
		coreharness.HooksOnly(coreharness.HookFeedsChanged, coreharness.HookCatalogRow))

	// ── Edge: one produce line running SYN-FNP-RUN, HMI setup on, its recipe named ──
	edge := edgeharness.NewEdge(t, edgeStation)
	processID, err := edge.DB.CreateProcess("SYN-FNP-PROC", "", "", "", false)
	mustNil(t, err, "process")
	stationID, err := edge.DB.CreateOperatorStation(stations.Input{
		ProcessID: processID, Code: "SYN-FNP-ST", Name: operator, Sequence: 1, Enabled: true,
	})
	mustNil(t, err, "station")
	nodeID, err := edge.DB.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, OperatorStationID: &stationID, CoreNodeName: line,
		Code: "FNP1", Name: line, Sequence: 1, Enabled: true,
	})
	mustNil(t, err, "process node")
	_, err = edge.DB.EnsureProcessNodeRuntime(nodeID)
	mustNil(t, err, "runtime")
	runningStyle, err := edge.DB.CreateStyle("SYN-FNP-RUN-STYLE", "", processID)
	mustNil(t, err, "running style")
	mustNil(t, edge.DB.SetActiveStyle(processID, &runningStyle), "active style")
	runningClaim, err := edge.DB.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
		StyleID: runningStyle, CoreNodeName: line, Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeTwoRobot,
		PayloadCode: running, UOPCapacity: 40, InboundSource: empties, InboundStaging: spot, OutboundDestination: dest,
	})
	mustNil(t, err, "running claim")
	mustNil(t, edge.DB.SetProcessNodeRuntime(nodeID, &runningClaim, 30), "runtime claim")
	_, err = edge.DB.PutRoutingNodes(domain.CoreNodeKinds{}, processID, []domain.RoutingNodeInput{
		{CoreNodeName: empties, Role: domain.RoutingRoleSource},
		{CoreNodeName: spot, Role: domain.RoutingRoleStaging},
		{CoreNodeName: dest, Role: domain.RoutingRoleDestination},
	}, nil, "syn-fnp")
	mustNil(t, err, "routing set")
	mustNil(t, edge.DB.SetFlowComposerEnabled(processID, true), "HMI setup on")
	recipes := edgeservice.NewProcessService(edge.DB)
	offer, err := recipes.PresetsFor(processID)
	mustNil(t, err, "the recipe offer")
	if len(offer.Candidates) != 1 {
		t.Fatalf("the recipe offer has %d shapes, want the running style's one", len(offer.Candidates))
	}
	recipeID, err := recipes.CreateFromCandidate(processID, "Standard", offer.Candidates[0].ShapeKey, "syn-fnp")
	mustNil(t, err, "name the recipe")

	// The carrier types and carrier rule Core's node list carries (Family as
	// Core builds it: every type but the bare markers, by its family). A stand-in
	// for the node feed, which this scenario does not route: the list is
	// applied through the engine's own applier, built from Core's rows.
	types, err := coreDB.ListBinTypes()
	mustNil(t, err, "Core's carrier types")
	family := map[string]string{}
	for _, bt := range types {
		if !bt.Bare {
			family[bt.Code] = bt.FamilyCode
		}
	}
	pairs, err := coreDB.ListPayloadBinTypeMappings()
	mustNil(t, err, "Core's carrier rule")
	var rule []protocol.PayloadBinTypeInfo
	for _, p := range pairs {
		rule = append(rule, protocol.PayloadBinTypeInfo{PayloadCode: p[0], BinTypeCode: p[1]})
	}
	edge.Engine.ApplyNodeList(&protocol.NodeListResponse{Family: family, PayloadBinTypes: rule})

	// ── the Bus, and Core's ask server named on the Edge's ack ──
	bus := harness.NewBus(t,
		harness.EdgeSide{EdgeStore: edge.DB, EdgeIngestor: edge.Ingestor},
		harness.CoreSide{CoreStore: coreDB, CoreIngestor: coreSide.Ingestor},
	)
	push := harness.StartBinsPush(t, coreDB, edge.Engine)
	bus.SetBinsPush(push)
	push.AnswerAsks(coreSide.AskKinds)
	settle := func() {
		for i := 0; i < 40; i++ {
			if bus.PumpAll() == 0 {
				time.Sleep(50 * time.Millisecond)
				if bus.PumpAll() == 0 {
					return
				}
			}
		}
	}
	eventually := func(what string, ok func() bool) {
		t.Helper()
		for i := 0; i < 60; i++ {
			settle()
			if ok() {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for: %s", what)
	}
	// submit is the operator's submit at the picker: CreatePart waits up to the
	// Edge's ask wait for Core's answer while the Bus carries the ask and the
	// answer.
	req := protocol.PartCreateRequest{PartNumber: newPart, StandardPack: 40, BinTypeCode: cart}
	submit := func() (*protocol.PartCreateResponse, error) {
		t.Helper()
		type answer struct {
			res *protocol.PartCreateResponse
			err error
		}
		done := make(chan answer, 1)
		go func() {
			res, err := edge.Engine.CreatePart(processID, req)
			done <- answer{res, err}
		}()
		for {
			select {
			case a := <-done:
				settle()
				return a.res, a.err
			case <-time.After(10 * time.Millisecond):
				bus.PumpAll()
			}
		}
	}
	settle()

	// ── SUBMIT: the tap waits for Core's word (engine CreatePart) ──
	made, err := submit()
	switch {
	case err != nil:
		t.Fatalf("the submit: %v", err)
	case made.Repeat || made.ProcessID != processID || made.Payload.Code != newPart || made.Payload.UOPCapacity != 40 ||
		len(made.BinTypes) != 1 || made.BinTypes[0] != (protocol.PayloadBinTypeInfo{PayloadCode: newPart, BinTypeCode: cart}):
		t.Fatalf("Core's answer = %+v, want the part made for process %d on %s", made, processID, cart)
	}

	// At Core: the part, its carrier, one manifest line at ratio 1, and who made it.
	part, err := coreDB.GetPayloadByCode(newPart)
	mustNil(t, err, "the part at Core")
	if part.UOPCapacity != 40 {
		t.Errorf("Core's standard pack = %d, want 40", part.UOPCapacity)
	}
	carriers, err := coreDB.ListBinTypesForPayload(part.ID)
	mustNil(t, err, "the part's carriers at Core")
	if len(carriers) != 1 || carriers[0].Code != cart {
		t.Errorf("Core's carriers for the part = %v, want %s alone", carriers, cart)
	}
	lines, err := coreDB.ListPayloadManifest(part.ID)
	mustNil(t, err, "the part's manifest at Core")
	if len(lines) != 1 || lines[0].PartNumber != newPart || lines[0].PartsPerCycle != 1 {
		t.Errorf("Core's manifest for the part = %+v, want one line of %s at ratio 1", lines, newPart)
	}
	madeBy := func() []string {
		t.Helper()
		rows, err := coreDB.ListAuditByAction(corestore.PayloadAuditEntity, coreservice.FloorCreateAction)
		mustNil(t, err, "the floor-create audit rows")
		var out []string
		for _, r := range rows {
			var rec struct {
				StationUID string `json:"station_uid"`
				ProcessID  int64  `json:"process_id"`
				Part       string `json:"part_number"`
			}
			mustNil(t, json.Unmarshal([]byte(r.NewValue), &rec), "the audit row's record")
			if r.EntityID != part.ID || r.Actor != edgeStation || rec.StationUID != edgeStation ||
				rec.ProcessID != processID || rec.Part != newPart {
				t.Errorf("audit row %+v (record %+v), want part %d made by %s for process %d",
					r, rec, part.ID, edgeStation, processID)
			}
			out = append(out, r.Actor)
		}
		return out
	}
	if rows := madeBy(); len(rows) != 1 {
		t.Fatalf("floor-create audit rows = %v, want one naming %s", rows, edgeStation)
	}

	// At the Edge: the catalog row, the part on the process, the carrier rule.
	entry, err := edge.DB.GetPayloadCatalogByCode(newPart)
	mustNil(t, err, "the Edge's catalog row")
	if entry.ID != part.ID || entry.UOPCapacity != 40 || len(entry.Manifest) != 1 ||
		entry.Manifest[0].PartNumber != newPart || entry.Manifest[0].PartsPerCycle != 1 {
		t.Errorf("the Edge's catalog row = %+v, want Core's part %d, pack 40, one line at ratio 1", entry, part.ID)
	}
	onProcess := func() int {
		t.Helper()
		codes, err := edge.DB.ListProcessPayloads(processID)
		mustNil(t, err, "the process's parts")
		n := 0
		for _, c := range codes {
			if c == newPart {
				n++
			}
		}
		return n
	}
	if n := onProcess(); n != 1 {
		t.Errorf("the process's part set names %s %d times, want once", newPart, n)
	}
	if got := edge.Engine.BinTypeForPayload(newPart); got != cart {
		t.Errorf("the Edge's carrier rule for %s = %q, want %s", newPart, got, cart)
	}
	if got := edge.Engine.BinTypeForPayload(running); got != cart {
		t.Errorf("the carrier rule lost the running part's row: %q, want %s", got, cart)
	}

	// ── MINT: the recipe with the new part, from the station ──
	minted, err := edge.Engine.MintStyle(processID, edgeengine.MintRequest{
		PresetID: recipeID, Parts: []edgeengine.PartPick{{Code: newPart}}, StationID: stationID,
		Source: domain.ClaimSourceHMI, CalledBy: operator,
	})
	mustNil(t, err, "the mint")
	if !minted.Minted || minted.StyleID == runningStyle {
		t.Fatalf("the mint = %+v, want a new style", minted)
	}
	claims, err := edge.DB.ListStyleNodeClaims(minted.StyleID)
	mustNil(t, err, "the minted style's claims")
	if len(claims) != 1 || claims[0].CoreNodeName != line || claims[0].PayloadCode != newPart {
		t.Fatalf("the minted style's claims = %+v, want %s on %s", claims, newPart, line)
	}

	// ── START the changeover to it ──
	_, err = edge.Engine.StartProcessChangeover(processID, minted.StyleID, operator, "")
	mustNil(t, err, "the changeover's start")
	open, err := edge.DB.GetActiveProcessChangeover(processID)
	mustNil(t, err, "the open changeover")
	if open.FromStyleID == nil || *open.FromStyleID != runningStyle || open.ToStyleID != minted.StyleID {
		t.Errorf("the open changeover runs %v -> %d, want %d -> %d", open.FromStyleID, open.ToStyleID, runningStyle, minted.StyleID)
	}
	tasks, err := edge.DB.ListChangeoverNodeTasks(open.ID)
	mustNil(t, err, "the changeover's node tasks")
	if len(tasks) != 1 || tasks[0].ProcessNodeID != nodeID || tasks[0].FromClaimID == nil || *tasks[0].FromClaimID != runningClaim ||
		tasks[0].ToClaimID == nil || *tasks[0].ToClaimID != claims[0].ID ||
		tasks[0].NextMaterialOrderID == nil || tasks[0].OldMaterialReleaseOrderID == nil {
		t.Fatalf("the changeover's node tasks = %+v, want one at %s from claim %d to the minted claim %d with both legs ordered",
			tasks, line, runningClaim, claims[0].ID)
	}
	// The legs: the supply brings an empty for the new part, the evacuation
	// takes the running part's bin away. Core is given what both need (the
	// line's bin, an empty carrier at the source), so each goes to the fleet
	// holding its bin; the scenario drives them no further. The supply's empty
	// is found by the carrier rule the floor create wrote.
	leg := func(id int64) *coreorders.Order {
		t.Helper()
		o, err := edge.DB.GetOrder(id)
		mustNil(t, err, "the Edge's leg")
		var c *coreorders.Order
		eventually("leg "+o.UUID+" with the fleet", func() bool {
			c, err = coreDB.GetOrderByUUID(o.UUID)
			return err == nil && c.VendorOrderID != ""
		})
		if o.PayloadCode != c.PayloadCode {
			t.Errorf("leg %d names %s at the Edge and %s at Core", id, o.PayloadCode, c.PayloadCode)
		}
		return c
	}
	supply, evac := leg(*tasks[0].NextMaterialOrderID), leg(*tasks[0].OldMaterialReleaseOrderID)
	if supply.PayloadCode != newPart || coreCart(supply) == nil || *coreCart(supply) != empty.ID {
		t.Errorf("the supply leg names %s holding bin %v, want %s holding the empty %d",
			supply.PayloadCode, coreCart(supply), newPart, empty.ID)
	}
	if evac.PayloadCode != running || coreCart(evac) == nil || *coreCart(evac) != lineBin.ID {
		t.Errorf("the evacuation names %s holding bin %v, want %s holding the line's bin %d",
			evac.PayloadCode, coreCart(evac), running, lineBin.ID)
	}

	// ── REPEAT: the same submit again ──
	again, err := submit()
	mustNil(t, err, "the repeat")
	if !again.Repeat || again.Payload.ID != part.ID {
		t.Errorf("the repeat's answer = %+v, want Core's part %d as a repeat", again, part.ID)
	}
	payloads, err := coreDB.ListPayloads()
	mustNil(t, err, "Core's parts")
	n := 0
	for _, p := range payloads {
		if p.Code == newPart {
			n++
		}
	}
	if n != 1 {
		t.Errorf("Core holds %d parts %s after the repeat, want 1", n, newPart)
	}
	if rows := madeBy(); len(rows) != 1 {
		t.Errorf("floor-create audit rows after the repeat = %v, want the first alone", rows)
	}
	if n := onProcess(); n != 1 {
		t.Errorf("the process's part set names %s %d times after the repeat, want once", newPart, n)
	}
}
