//go:build docker

package main

// bus_order_docker_test.go — the order Core's bus subscribers run in, heard on
// a real engine whose three subscriber sets are made as main makes them: the
// engine's own at Start, the bins feed's after it, the web layer's when the
// router is built. TestSubscriberCensus (engine) holds where each subscription
// is made; this holds what that order does on one cancel and one delivery.
//
// One ordered stream: the bins feed's hooks write to the debug log the router
// is built with, and the router hands each entry to its event hub as a
// debug-log frame (www.NewRouter: dbg.SetOnEntry), on the emitter's goroutine,
// as each screen's subscriber hands the hub its frame. So for one event the
// hook calls and the frames land on the hub's one queue in the order their
// subscribers ran, and a browser tab reads them in that order.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/debuglog"
	"shingocore/config"
	"shingocore/dispatch"
	"shingocore/engine"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/www"
)

// busOrderTranscript is what the feed's hooks and a browser tab hear, in the
// order they hear it, for a retrieve cancelled after dispatch (bus-cancel) and
// a retrieve of the same bin driven to the line and received (bus-deliver).
// For every event both hear, the feed (main's set) hears it before the screens
// (the web layer's set).
var busOrderTranscript = []string{
	"feed   started       bus-cancel [LINE1-IN]", // received: its delivery node
	"screen order-update  bus-cancel received",
	"feed   started       bus-cancel [STORAGE-A1 LINE1-IN]", // dispatched: source, then destination
	"screen order-update  bus-cancel dispatched",
	"feed   ended         bus-cancel",
	"screen order-update  bus-cancel cancelled",
	"feed   started       bus-deliver [LINE1-IN]",
	"screen order-update  bus-deliver received",
	"feed   started       bus-deliver [STORAGE-A1 LINE1-IN]",
	"screen order-update  bus-deliver dispatched",
	// A status change: the order-update frame's subscriber, then the
	// mission-event frame's.
	"screen order-update  bus-deliver status_changed RUNNING",
	"screen mission-event bus-deliver CREATED->RUNNING",
	// FINISHED: the engine's status handler, the first to hear it, moves the
	// bin and marks the order delivered inside its own call
	// (wiring_vendor_status.go, the delivered arm), so every set hears the bin
	// and the completion before the screens hear the status change itself.
	"feed   changed       BIN-BUS [LINE1-IN STORAGE-A1 LINE1-IN]", // the bin's node, from, to
	"screen bin-update    BIN-BUS moved at LINE1-IN",
	"feed   ended         bus-deliver",
	"screen order-update  bus-deliver completed",
	"screen order-update  bus-deliver status_changed FINISHED",
	"screen mission-event bus-deliver RUNNING->FINISHED",
	// The receipt: EventOrderCompleted again, on delivered to confirmed.
	"feed   ended         bus-deliver",
	"screen order-update  bus-deliver completed",
}

// TestBusOrder_ADeliveryAndACancel pins the order Core's subscribers run in on
// a real engine, for the events more than one set hears.
func TestBusOrder_ADeliveryAndACancel(t *testing.T) {
	db := testdb.Open(t)
	storage, line, payload := testdb.SetupTestData(t, db)
	testdb.CreateBinAtNode(t, db, payload.Code, storage.ID, "BIN-BUS")

	sim := simulator.New()
	eng := engine.New(engine.Config{AppConfig: config.Defaults(), DB: db, Fleet: sim, LogFunc: t.Logf})

	// main's order (main.go): Start, the bins feed, then the router.
	eng.Start()
	t.Cleanup(eng.Stop)
	dbg, err := debuglog.New(1024, nil)
	if err != nil {
		t.Fatalf("debug log: %v", err)
	}
	dbg.SetStderr(nil)
	feed := &feedCalls{log: dbg.Func(feedSubsystem)}
	eng.SubscribeBinsFeed(engine.BinsFeedHooks{
		Changed: func(binID int64, nodeIDs ...int64) { feed.add(feedCall{"changed", 0, binID, nodeIDs, nil}) },
		Started: func(orderID int64, names ...string) { feed.add(feedCall{"started", orderID, 0, nil, names}) },
		Ended:   func(orderID int64) { feed.add(feedCall{"ended", orderID, 0, nil, nil}) },
	})
	handler, stopWeb, err := www.NewRouter(eng, www.NewDoors(eng), dbg)
	if err != nil {
		t.Fatalf("build router: %v", err)
	}
	t.Cleanup(stopWeb)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	frames := openTab(t, srv.URL+"/events?topics=order-update,mission-event,bin-update,debug-log")

	d := eng.Dispatcher()
	env := testdb.Envelope()
	retrieve := func(uuid string) *protocol.OrderRequest {
		return &protocol.OrderRequest{OrderUUID: uuid, OrderType: dispatch.OrderTypeRetrieve,
			PayloadCode: payload.Code, DeliveryNode: line.Name, Quantity: 1}
	}
	// The cancel: dispatched, then cancelled before the robot moves.
	d.HandleOrderRequest(env, retrieve("bus-cancel"))
	testdb.RequireOrderStatus(t, db, "bus-cancel", protocol.StatusDispatched)
	d.HandleOrderCancel(env, &protocol.OrderCancel{OrderUUID: "bus-cancel", Reason: "pin"})
	testdb.RequireOrderStatus(t, db, "bus-cancel", protocol.StatusCancelled)
	// The delivery: the same bin, driven to the line and received there.
	d.HandleOrderRequest(env, retrieve("bus-deliver"))
	o := testdb.RequireOrderStatus(t, db, "bus-deliver", protocol.StatusDispatched)
	sim.DriveState(o.VendorOrderID, "RUNNING")
	testdb.RequireOrderStatus(t, db, "bus-deliver", protocol.StatusInTransit)
	sim.DriveState(o.VendorOrderID, "FINISHED")
	testdb.RequireOrderStatus(t, db, "bus-deliver", protocol.StatusDelivered)
	d.HandleOrderReceipt(env, &protocol.OrderReceipt{OrderUUID: "bus-deliver", ReceiptType: "confirmed", FinalCount: 1})
	testdb.RequireOrderStatus(t, db, "bus-deliver", protocol.StatusConfirmed)
	// Last on the hub's queue: when the tab reads it, it has read everything
	// the drive above put there.
	dbg.Log(feedSubsystem, "end")

	got := readTranscript(t, frames, feed, names{db})
	if len(got) == 0 {
		t.Fatal("the tab heard nothing: a transcript that hears nothing pins nothing")
	}
	if !slices.Equal(got, busOrderTranscript) {
		t.Errorf("Core's subscribers ran in another order on a cancel and a delivery, or an event came or went.\n"+
			"A moved subscription fails TestSubscriberCensus (engine) too, and a move needs the owner's word first "+
			"(AGENTS.md, Stop and ask before); with it, re-record busOrderTranscript from the got column. An event "+
			"added or dropped is a behaviour change of its own: name it in the commit, then re-record.\n%s",
			sideBySide(busOrderTranscript, got))
	}
}

const feedSubsystem = "bins feed"

// feedCall is one call of a bins-feed hook, as the hook was given it.
type feedCall struct {
	hook    string
	orderID int64
	binID   int64
	nodeIDs []int64
	names   []string
}

// feedCalls records the hook calls; each logs its index, which reaches the
// tab as a debug-log frame at the place the hook ran.
type feedCalls struct {
	mu    sync.Mutex
	calls []feedCall
	log   func(string, ...any)
}

func (f *feedCalls) add(c feedCall) {
	f.mu.Lock()
	i := len(f.calls)
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	f.log("call %d", i)
}

func (f *feedCalls) call(i int) (feedCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i < 0 || i >= len(f.calls) {
		return feedCall{}, false
	}
	return f.calls[i], true
}

type sseFrame struct{ event, data string }

// openTab reads /events as a browser tab does, from its first frame.
func openTab(t *testing.T, url string) <-chan sseFrame {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel) // before srv.Close: the handler returns when the tab goes
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("tab request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open the tab: %v", err)
	}
	frames := make(chan sseFrame, 1024)
	go func() {
		defer close(frames)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		var f sseFrame
		for sc.Scan() {
			switch line := sc.Text(); {
			case line == "":
				if f.event != "" {
					frames <- f
				}
				f = sseFrame{}
			case strings.HasPrefix(line, "event: "):
				f.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				f.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	// The hub has the tab as a client before it writes connected.
	select {
	case f, ok := <-frames:
		if !ok || f.event != "connected" {
			t.Fatalf("the tab's first frame = %q (open %v), want connected", f.event, ok)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the tab was not connected in 10 s")
	}
	return frames
}

// names renders the ids a frame or a hook call carries by what a reader
// knows them by: an order by its uuid, a bin by its label, a node by its name.
type names struct{ db *store.DB }

func (n names) order(id int64) string {
	if o, err := n.db.GetOrder(id); err == nil {
		return o.EdgeUUID
	}
	return fmt.Sprintf("order#%d", id)
}

func (n names) bin(id int64) string {
	if b, err := n.db.GetBin(id); err == nil {
		return b.Label
	}
	return fmt.Sprintf("bin#%d", id)
}

func (n names) node(id int64) string {
	if id == 0 {
		return "-"
	}
	if nd, err := n.db.GetNode(id); err == nil {
		return nd.Name
	}
	return fmt.Sprintf("node#%d", id)
}

// readTranscript reads the tab up to the end marker and renders each frame.
func readTranscript(t *testing.T, frames <-chan sseFrame, feed *feedCalls, n names) []string {
	t.Helper()
	var got []string
	deadline := time.After(30 * time.Second)
	for {
		var f sseFrame
		select {
		case fr, ok := <-frames:
			if !ok {
				t.Fatalf("the tab closed before the end marker; read so far:\n  %s", strings.Join(got, "\n  "))
			}
			f = fr
		case <-deadline:
			t.Fatalf("no end marker in 30 s; read so far:\n  %s", strings.Join(got, "\n  "))
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(f.data), &m); err != nil {
			t.Fatalf("frame %s: data is not JSON: %v (%s)", f.event, err, f.data)
		}
		id := func(key string) int64 { v, _ := m[key].(float64); return int64(v) }
		str := func(key string) string { s, _ := m[key].(string); return s }
		switch f.event {
		case "debug-log":
			if str("subsystem") != feedSubsystem {
				continue
			}
			msg := str("message")
			if msg == "end" {
				return got
			}
			var i int
			if _, err := fmt.Sscanf(msg, "call %d", &i); err != nil {
				t.Fatalf("feed frame %q: %v", msg, err)
			}
			c, ok := feed.call(i)
			if !ok {
				t.Fatalf("feed frame %q names no recorded call", msg)
			}
			switch c.hook {
			case "changed":
				nodes := make([]string, len(c.nodeIDs))
				for j, id := range c.nodeIDs {
					nodes[j] = n.node(id)
				}
				got = append(got, fmt.Sprintf("feed   changed       %s %v", n.bin(c.binID), nodes))
			case "started":
				got = append(got, fmt.Sprintf("feed   started       %s %v", n.order(c.orderID), c.names))
			default:
				got = append(got, fmt.Sprintf("feed   ended         %s", n.order(c.orderID)))
			}
		case "order-update":
			line := fmt.Sprintf("screen order-update  %s %s", n.order(id("order_id")), str("type"))
			if s := str("new_status"); s != "" {
				line += " " + s
			}
			got = append(got, line)
		case "mission-event":
			got = append(got, fmt.Sprintf("screen mission-event %s %s->%s", n.order(id("order_id")), str("old_state"), str("new_state")))
		case "bin-update":
			got = append(got, fmt.Sprintf("screen bin-update    %s %s at %s", n.bin(id("bin_id")), str("action"), n.node(id("node_id"))))
		}
	}
}

// sideBySide lists want and got row by row, marking the rows that differ.
func sideBySide(want, got []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "    %-58s | %s\n", "want", "got")
	for i := 0; i < len(want) || i < len(got); i++ {
		w, g := "", ""
		if i < len(want) {
			w = want[i]
		}
		if i < len(got) {
			g = got[i]
		}
		mark := "  "
		if w != g {
			mark = "!="
		}
		fmt.Fprintf(&b, "%s %2d %-55s | %s\n", mark, i+1, w, g)
	}
	return b.String()
}
