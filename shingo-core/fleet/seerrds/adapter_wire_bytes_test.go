package seerrds

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"shingocore/fleet"
)

// THE BYTES CORE SENDS RDS, PINNED AT THE BODY.
//
// The create (/setOrder), the append (/addBlocks) and the fleet test command
// (executeOrderCommand) are captured as the raw request bodies the adapter
// posts, and compared byte for byte. The blocks are the ones the dispatcher
// sends: JackLoad, JackUnload and Wait at a node, and the seeded advanced load
// sequence's four names at one location (stepsToBlocks). A field added to
// fleet.OrderBlock for Core's own use must leave every body here unchanged.

// wireCreateBlocks is a create as the dispatcher builds one: the seeded load
// sequence at the source, a wait at the line, a set-down.
var wireCreateBlocks = []fleet.OrderBlock{
	{BlockID: "sg-7-abcd1234-b1", Location: "SRC-01", BinTask: "Go_AP1"},
	{BlockID: "sg-7-abcd1234-b1-1", Location: "SRC-01", BinTask: "Spin_90"},
	{BlockID: "sg-7-abcd1234-b1-2", Location: "SRC-01", BinTask: "load"},
	{BlockID: "sg-7-abcd1234-b1-3", Location: "SRC-01", BinTask: "Spin_inverse_90"},
	{BlockID: "sg-7-abcd1234-b2", Location: "STG-01", BinTask: "JackUnload"},
	{BlockID: "sg-7-abcd1234-b3", Location: "LINE-01", BinTask: "Wait"},
}

// wireAppendBlocks is the tail a release appends after that wait.
var wireAppendBlocks = []fleet.OrderBlock{
	{BlockID: "sg-7-abcd1234-b4", Location: "LINE-01", BinTask: "JackLoad"},
	{BlockID: "sg-7-abcd1234-b5", Location: "OUT-01", BinTask: "JackUnload"},
}

const wantSetOrderBody = `{"id":"sg-7-abcd1234","externalId":"edge-uuid-7","group":"heavy",` +
	`"keyRoute":["LM1"],"keyTask":"load","priority":3,"blocks":[` +
	`{"blockId":"sg-7-abcd1234-b1","location":"SRC-01","binTask":"Go_AP1","goodsId":"sg-7-abcd1234_goods"},` +
	`{"blockId":"sg-7-abcd1234-b1-1","location":"SRC-01","binTask":"Spin_90","goodsId":"sg-7-abcd1234_goods"},` +
	`{"blockId":"sg-7-abcd1234-b1-2","location":"SRC-01","binTask":"load","goodsId":"sg-7-abcd1234_goods"},` +
	`{"blockId":"sg-7-abcd1234-b1-3","location":"SRC-01","binTask":"Spin_inverse_90","goodsId":"sg-7-abcd1234_goods"},` +
	`{"blockId":"sg-7-abcd1234-b2","location":"STG-01","binTask":"JackUnload","goodsId":"sg-7-abcd1234_goods"},` +
	`{"blockId":"sg-7-abcd1234-b3","location":"LINE-01","binTask":"Wait","goodsId":"sg-7-abcd1234_goods"}` +
	`],"complete":false}`

const wantAddBlocksBody = `{"id":"sg-7-abcd1234","blocks":[` +
	`{"blockId":"sg-7-abcd1234-b4","location":"LINE-01","binTask":"JackLoad"},` +
	`{"blockId":"sg-7-abcd1234-b5","location":"OUT-01","binTask":"JackUnload"}` +
	`],"complete":true,"vehicle":"AMR-01"}`

// wireBodies stands up a fake RDS that keeps the raw body of every POST by
// path and names AMR-01 on the order's details, so an append pins a vehicle.
func wireBodies(t *testing.T) (*Adapter, func(path string) []string) {
	t.Helper()
	var mu sync.Mutex
	bodies := map[string][]string{}
	reply := func(w http.ResponseWriter, body string) {
		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("fake RDS reply: %v", err)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, ok := strings.CutPrefix(r.URL.Path, "/orderDetails/"); ok {
			reply(w, `{"code":0,"msg":"ok","id":"`+id+`","state":"WAITING","vehicle":"AMR-01"}`)
			return
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read %s body: %v", r.URL.Path, err)
		}
		mu.Lock()
		bodies[r.URL.Path] = append(bodies[r.URL.Path], string(raw))
		mu.Unlock()
		reply(w, `{"code":0,"msg":"ok"}`)
	}))
	t.Cleanup(srv.Close)
	a := New(Config{BaseURL: srv.URL, Timeout: 5 * time.Second, DebugLog: func(string, ...any) {}})
	return a, func(path string) []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies[path]...)
	}
}

// sendWireOrder posts create as a staged create and appendBlocks as its sealing
// append, through the adapter, and returns the two bodies.
func sendWireOrder(t *testing.T, create, appendBlocks []fleet.OrderBlock) (setOrder, addBlocks string) {
	t.Helper()
	a, bodies := wireBodies(t)
	if _, err := a.CreateOrder(fleet.CreateOrderRequest{
		OrderID:    "sg-7-abcd1234",
		ExternalID: "edge-uuid-7",
		Blocks:     create,
		Priority:   3,
		RobotGroup: "heavy",
		KeyRoute:   []string{"LM1"},
		KeyTask:    "load",
		Complete:   false,
	}); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if err := a.ReleaseOrder("sg-7-abcd1234", appendBlocks, true); err != nil {
		t.Fatalf("ReleaseOrder: %v", err)
	}
	sets, adds := bodies("/setOrder"), bodies("/addBlocks")
	if len(sets) != 1 || len(adds) != 1 {
		t.Fatalf("posted %d /setOrder and %d /addBlocks bodies, want one of each", len(sets), len(adds))
	}
	return sets[0], adds[0]
}

func TestWireBytes_CreateAndAppendBodies(t *testing.T) {
	t.Parallel()
	setOrder, addBlocks := sendWireOrder(t, wireCreateBlocks, wireAppendBlocks)
	if setOrder != wantSetOrderBody {
		t.Errorf("the /setOrder body changed.\n got: %s\nwant: %s", setOrder, wantSetOrderBody)
	}
	if addBlocks != wantAddBlocksBody {
		t.Errorf("the /addBlocks body changed.\n got: %s\nwant: %s", addBlocks, wantAddBlocksBody)
	}
}

// The fleet test command is one of the three orderless doors: its block names
// a location and no binTask, and its vendor order is never tracked, so no
// block of it reaches the engine's block subscriber.
func TestWireBytes_TestCommandSendsNoBinTask(t *testing.T) {
	t.Parallel()
	a, bodies := wireBodies(t)
	res, err := a.ExecuteVendorCommand(fleet.VendorCommand{Type: "move", RobotID: "AMR-02", Location: "PARK-01"})
	if err != nil {
		t.Fatalf("ExecuteVendorCommand: %v", err)
	}
	sets := bodies("/setOrder")
	if len(sets) != 1 {
		t.Fatalf("posted %d /setOrder bodies, want one", len(sets))
	}
	want := `{"id":"` + res.VendorOrderID + `","vehicle":"AMR-02","blocks":[{"blockId":"` + res.VendorOrderID +
		`-b1","location":"PARK-01"}],"complete":true}`
	if sets[0] != want {
		t.Errorf("the test command's /setOrder body changed.\n got: %s\nwant: %s", sets[0], want)
	}
}
