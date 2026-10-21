package www

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// sse_sequence_test.go — what makes "no timer on the board" honest: a lost
// frame is never silent. Every frame carries the hub's sequence number, the
// heartbeat says the number of the last frame the client should have seen, a
// client whose queue is full is evicted at once (its reconnect refetches), and
// a hub buffer that overflowed evicts everyone. The stream also honours
// ?topics=, touches its station, and carries one coalesced nudge per burst.

// sseFrame is one parsed frame off /events.
type sseFrame struct {
	event string
	id    string
	data  string
}

// sseFrames opens url and streams parsed frames until ctx ends.
func sseFrames(t *testing.T, ctx context.Context, url string) <-chan sseFrame {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	out := make(chan sseFrame, 256)
	go func() {
		defer resp.Body.Close()
		defer close(out)
		sc := bufio.NewScanner(resp.Body)
		var f sseFrame
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if f.event != "" {
					select {
					case out <- f:
					case <-ctx.Done():
						return
					}
				}
				f = sseFrame{}
			case strings.HasPrefix(line, "event: "):
				f.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "id: "):
				f.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				f.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return out
}

// nextFrame reads frames until one named want arrives or the timeout passes.
func nextFrame(t *testing.T, ch <-chan sseFrame, want string, timeout time.Duration) (sseFrame, bool) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case f, ok := <-ch:
			if !ok {
				return sseFrame{}, false
			}
			if f.event == want {
				return f, true
			}
		case <-deadline:
			return sseFrame{}, false
		}
	}
}

func frameSeq(t *testing.T, f sseFrame) int64 {
	t.Helper()
	var body struct {
		Seq *int64 `json:"seq"`
	}
	if err := json.Unmarshal([]byte(f.data), &body); err != nil || body.Seq == nil {
		t.Fatalf("%s frame carries no seq: %q", f.event, f.data)
	}
	return *body.Seq
}

func registered(h *EventHub, c *sseClient) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.clients[c]
	return ok
}

// TestSSE_FramesAndHeartbeatCarryTheHubSequence: each frame's id is the hub's
// number for it, rising; the connected frame and every heartbeat carry the
// number of the last frame queued for this client, so a client that saw fewer
// knows it lost one.
func TestSSE_FramesAndHeartbeatCarryTheHubSequence(t *testing.T) {
	hub := NewEventHub()
	hub.keepalive = 40 * time.Millisecond
	hub.Start()
	defer hub.Stop()
	srv := httptest.NewServer(http.HandlerFunc(hub.HandleSSE))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hub.Broadcast(SSEEvent{Type: "order-update", Data: 0})
	time.Sleep(20 * time.Millisecond)
	ch := sseFrames(t, ctx, srv.URL+"/events")
	conn, ok := nextFrame(t, ch, "connected", 2*time.Second)
	if !ok {
		t.Fatal("no connected frame")
	}
	start := frameSeq(t, conn)

	var ids []int64
	for i := 1; i <= 3; i++ {
		hub.Broadcast(SSEEvent{Type: "order-update", Data: i})
		f, ok := nextFrame(t, ch, "order-update", 2*time.Second)
		if !ok {
			t.Fatalf("frame %d never arrived", i)
		}
		id, err := strconv.ParseInt(f.id, 10, 64)
		if err != nil {
			t.Fatalf("order-update frame carries no id: %+v", f)
		}
		ids = append(ids, id)
	}
	if ids[0] <= start || ids[1] <= ids[0] || ids[2] <= ids[1] {
		t.Fatalf("frame ids %v must rise past the connected frame's seq %d", ids, start)
	}
	hb, ok := nextFrame(t, ch, "heartbeat", 2*time.Second)
	if !ok {
		t.Fatal("no heartbeat")
	}
	if got := frameSeq(t, hb); got != ids[2] {
		t.Fatalf("heartbeat seq = %d, want %d (the last frame queued for this client)", got, ids[2])
	}
}

// TestSSE_HeartbeatSeqCountsOnlyTheClientsTopics: a frame the client did not
// ask for is not one it lost, so it does not move the client's number.
func TestSSE_HeartbeatSeqCountsOnlyTheClientsTopics(t *testing.T) {
	hub := NewEventHub()
	hub.keepalive = 40 * time.Millisecond
	hub.Start()
	defer hub.Stop()
	srv := httptest.NewServer(http.HandlerFunc(hub.HandleSSE))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := sseFrames(t, ctx, srv.URL+"/events?topics=bins,order-update")
	if _, ok := nextFrame(t, ch, "connected", 2*time.Second); !ok {
		t.Fatal("no connected frame")
	}
	hub.Broadcast(SSEEvent{Type: "bins", Data: 1})
	got, ok := nextFrame(t, ch, "bins", 2*time.Second)
	if !ok {
		t.Fatal("bins never arrived")
	}
	hub.Broadcast(SSEEvent{Type: "plc-status", Data: 2})
	hub.Broadcast(SSEEvent{Type: "counter-update", Data: 3})
	time.Sleep(20 * time.Millisecond)
	hb, ok := nextFrame(t, ch, "heartbeat", 2*time.Second)
	if !ok {
		t.Fatal("no heartbeat")
	}
	if seq := frameSeq(t, hb); strconv.FormatInt(seq, 10) != got.id {
		t.Fatalf("heartbeat seq = %d, want %s: frames outside the client's topics moved it", seq, got.id)
	}
}

// TestSSE_HonoursTopics: a client that names topics receives those and the
// stream's own frames, nothing else. A client that names none receives every
// durable topic, as before.
func TestSSE_HonoursTopics(t *testing.T) {
	hub := NewEventHub()
	hub.Start()
	defer hub.Stop()
	srv := httptest.NewServer(http.HandlerFunc(hub.HandleSSE))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	scoped := sseFrames(t, ctx, srv.URL+"/events?topics=order-update")
	all := sseFrames(t, ctx, srv.URL+"/events")
	for _, ch := range []<-chan sseFrame{scoped, all} {
		if _, ok := nextFrame(t, ch, "connected", 2*time.Second); !ok {
			t.Fatal("no connected frame")
		}
	}
	hub.Broadcast(SSEEvent{Type: "plc-status", Data: 1})
	hub.Broadcast(SSEEvent{Type: "order-update", Data: 2})
	f, ok := nextFrame(t, scoped, "order-update", 2*time.Second)
	if !ok {
		t.Fatal("the scoped client lost the topic it asked for")
	}
	_ = f
	if _, ok := nextFrame(t, all, "plc-status", 2*time.Second); !ok {
		t.Fatal("a client with no topics lost plc-status")
	}
	// plc-status was broadcast before order-update and frames are ordered, so
	// had it been sent to the scoped client it would have been read above.
	hub.Broadcast(SSEEvent{Type: "plc-status", Data: 3})
	if _, ok := nextFrame(t, scoped, "plc-status", 200*time.Millisecond); ok {
		t.Fatal("the scoped client received plc-status, which it did not ask for")
	}
}

// TestSSE_EvictsAClientOnItsFirstDurableDrop (sse.go's per-client fan-out): a
// full queue was a silent loss for up to ten frames. It is an eviction now, at
// once: the stream ends, the client reconnects and refetches.
func TestSSE_EvictsAClientOnItsFirstDurableDrop(t *testing.T) {
	h := NewEventHub()
	h.Start()
	defer h.Stop()
	c := &sseClient{events: make(chan SSEEvent, 1), lossy: make(chan SSEEvent, 1)}
	h.register(c)
	defer h.unregister(c)

	h.Broadcast(SSEEvent{Type: "order-update", Data: 1})
	h.Broadcast(SSEEvent{Type: "order-update", Data: 2})
	deadline := time.Now().Add(2 * time.Second)
	for registered(h, c) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if registered(h, c) {
		t.Fatal("a client whose queue dropped a durable frame is still registered: the frame was lost silently")
	}
}

// TestSSE_HubOverflowEvictsEveryClient (sse.go's Broadcast): a durable frame the
// hub's own buffer could not take was dropped for every client with a log line.
// Nobody can tell whether it was theirs, so everyone reconnects and refetches.
func TestSSE_HubOverflowEvictsEveryClient(t *testing.T) {
	h := NewEventHub()
	a := &sseClient{events: make(chan SSEEvent, 512), lossy: make(chan SSEEvent, 1)}
	b := &sseClient{events: make(chan SSEEvent, 512), lossy: make(chan SSEEvent, 1)}
	h.register(a)
	h.register(b)
	defer h.unregister(a)
	defer h.unregister(b)
	// Not started: nothing drains the buffer, so the 257th durable frame overflows.
	for i := range cap(h.broadcast) + 1 {
		h.Broadcast(SSEEvent{Type: "order-update", Data: i})
	}
	h.Start()
	defer h.Stop()
	deadline := time.Now().Add(2 * time.Second)
	for (registered(h, a) || registered(h, b)) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if registered(h, a) || registered(h, b) {
		t.Fatal("the hub dropped a durable frame and a client stayed connected, unaware")
	}
}

// TestSSE_HubOverflowOfDiagnosticsEvictsNobody: a lossy topic's overflow is the
// best-effort it always was.
func TestSSE_HubOverflowOfDiagnosticsEvictsNobody(t *testing.T) {
	h := NewEventHub()
	c := &sseClient{events: make(chan SSEEvent, 1), lossy: make(chan SSEEvent, 1), debug: true}
	h.register(c)
	defer h.unregister(c)
	for i := range cap(h.broadcast) + 50 {
		h.Broadcast(SSEEvent{Type: "debug-log", Data: i})
	}
	h.Start()
	defer h.Stop()
	time.Sleep(100 * time.Millisecond)
	if !registered(h, c) {
		t.Fatal("a diagnostic overflow evicted a client")
	}
}

// TestSSE_StreamTouchesItsStation: the Processes page's "screen online" is the
// stream's, on connect and on every heartbeat, now that no view poll runs.
func TestSSE_StreamTouchesItsStation(t *testing.T) {
	hub := NewEventHub()
	hub.keepalive = 30 * time.Millisecond
	var mu sync.Mutex
	var touched []int64
	hub.SetStationToucher(func(id int64) {
		mu.Lock()
		touched = append(touched, id)
		mu.Unlock()
	})
	hub.Start()
	defer hub.Stop()
	srv := httptest.NewServer(http.HandlerFunc(hub.HandleSSE))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := sseFrames(t, ctx, srv.URL+"/events?station=7&topics=bins")
	if _, ok := nextFrame(t, ch, "connected", 2*time.Second); !ok {
		t.Fatal("no connected frame")
	}
	for range 2 {
		if _, ok := nextFrame(t, ch, "heartbeat", 2*time.Second); !ok {
			t.Fatal("no heartbeat")
		}
	}
	plain := sseFrames(t, ctx, srv.URL+"/events")
	if _, ok := nextFrame(t, plain, "connected", 2*time.Second); !ok {
		t.Fatal("no connected frame")
	}
	mu.Lock()
	got := append([]int64(nil), touched...)
	mu.Unlock()
	if len(got) < 3 {
		t.Fatalf("touched %v: want station 7 on connect and on each of two heartbeats", got)
	}
	for _, id := range got {
		if id != 7 {
			t.Fatalf("touched %v: a stream that named no station touched one", got)
		}
	}
}

// TestSSE_NudgeIsCoalesced: a burst of writes is one nudge on the stream, the
// type every station takes.
func TestSSE_NudgeIsCoalesced(t *testing.T) {
	hub := NewEventHub()
	hub.nudgeWindow = 50 * time.Millisecond
	hub.Start()
	defer hub.Stop()
	c := &sseClient{events: make(chan SSEEvent, 64), lossy: make(chan SSEEvent, 1)}
	hub.register(c)
	defer hub.unregister(c)

	for range 20 {
		hub.Nudge()
	}
	time.Sleep(200 * time.Millisecond)
	n := 0
	for len(c.events) > 0 {
		if evt := <-c.events; evt.Type == sseNudgeTopic {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("20 nudges in one burst gave %d %s frames, want 1", n, sseNudgeTopic)
	}
	hub.Nudge()
	if !waitForType(c, sseNudgeTopic, time.Second) {
		t.Fatal("a nudge after the burst was not sent")
	}
}
