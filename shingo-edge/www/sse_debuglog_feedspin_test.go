package www

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sse_debuglog_feedspin_test.go — who receives a debug-log frame on /events.
//
// Today every connected client does: the topic is lossy (its own shallow
// queue) but not opt-in, so every page holding /events open — Production,
// Manual Order, Diagnostics, the station boards — receives every debug line.
//
// after (C1): only a client that connected with ?debug=1 receives debug-log;
// durable topics still reach every client.

// sseEventNames opens /events on srv and streams the `event:` names it
// receives until ctx ends.
func sseEventNames(t *testing.T, ctx context.Context, url string) <-chan string {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	out := make(chan string, 64)
	go func() {
		defer resp.Body.Close()
		defer close(out)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if name, ok := strings.CutPrefix(sc.Text(), "event: "); ok {
				select {
				case out <- name:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

// waitEvent reads names until want arrives or the timeout passes.
func waitEvent(ch <-chan string, want string, timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		select {
		case name, ok := <-ch:
			if !ok {
				return false
			}
			if name == want {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

func TestSSE_DebugLogReachesEveryClient_FeedsPin(t *testing.T) {
	hub := NewEventHub()
	hub.Start()
	defer hub.Stop()
	srv := httptest.NewServer(http.HandlerFunc(hub.HandleSSE))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	plain := sseEventNames(t, ctx, srv.URL+"/events")
	debug := sseEventNames(t, ctx, srv.URL+"/events?debug=1")
	// The connected frame is written after register, so both are in the hub.
	for name, ch := range map[string]<-chan string{"plain": plain, "debug": debug} {
		if !waitEvent(ch, "connected", 2*time.Second) {
			t.Fatalf("%s client never saw the connected frame", name)
		}
	}

	hub.Broadcast(SSEEvent{Type: "debug-log", Data: map[string]string{"msg": "feeds pin"}})

	cases := []struct {
		client string
		ch     <-chan string
		event  string
		want   bool
		after  bool
		label  string
	}{
		{"/events", plain, "debug-log", true, false, "C1"},
		{"/events?debug=1", debug, "debug-log", true, true, "C1"},
	}
	for _, c := range cases {
		if got := waitEvent(c.ch, c.event, 2*time.Second); got != c.want {
			t.Errorf("%s received %s = %v, want %v (after: %v, %s)", c.client, c.event, got, c.want, c.after, c.label)
		}
	}
	// Durable topics reach both clients, before and after C1.
	hub.Broadcast(SSEEvent{Type: "order-update", Data: map[string]int{"id": 1}})
	for name, ch := range map[string]<-chan string{"plain": plain, "debug": debug} {
		if !waitEvent(ch, "order-update", 2*time.Second) {
			t.Errorf("%s client did not receive order-update (after: same, C1)", name)
		}
	}
}
