package seerrds

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"shingocore/fleet"
)

// §R.98 stage A2 — the production twin of the simulator's never-issued shrug.
//
// The vehicle read is what stops RDS re-dispatching an appended tail to a
// different robot. When it did not answer, the adapter appended anyway with an
// empty pin. A read that failed and a read that named a robot produced the same
// append; so did RDS answering "I have no such order", which arrives here as an
// error from GetOrderDetails.

func tailBlocks() []fleet.OrderBlock {
	return []fleet.OrderBlock{
		{BlockID: "sg-9-tail", Location: "OUTBOUND-STG", BinTask: "JackUnload"},
	}
}

// A read that does not answer refuses, and nothing is appended.
func TestReleaseOrder_RefusesWhenVehicleReadFails(t *testing.T) {
	t.Parallel()
	var addBlocksCalls int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/orderDetails/sg-9-abc":
			w.WriteHeader(http.StatusInternalServerError)
		case "/addBlocks":
			atomic.AddInt32(&addBlocksCalls, 1)
			w.Write([]byte(`{"code":0,"msg":"ok"}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	adapter := New(Config{BaseURL: srv.URL, Timeout: 5 * time.Second, DebugLog: func(string, ...any) {}})

	err := adapter.ReleaseOrder("sg-9-abc", tailBlocks(), true)
	if err == nil {
		t.Fatal("a vehicle read that did not answer must refuse, not append blind")
	}
	if !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("the refusal must name which fact was missing; got %q", err)
	}
	if n := atomic.LoadInt32(&addBlocksCalls); n != 0 {
		t.Fatalf("nothing may be appended after a refusal; addBlocks called %d times", n)
	}
}

// RDS answering "no such order" arrives as a read error and takes the same arm —
// this is the plant's version of the simulator's never-issued mission.
func TestReleaseOrder_RefusesWhenRDSDoesNotHoldTheOrder(t *testing.T) {
	t.Parallel()
	var addBlocksCalls int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/orderDetails/sg-9-gone":
			// code=0 with no order id: RDS's shape for "nothing here".
			w.Write([]byte(`{"code":0,"msg":"ok"}`))
		case "/addBlocks":
			atomic.AddInt32(&addBlocksCalls, 1)
			w.Write([]byte(`{"code":0,"msg":"ok"}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	adapter := New(Config{BaseURL: srv.URL, Timeout: 5 * time.Second, DebugLog: func(string, ...any) {}})

	if err := adapter.ReleaseOrder("sg-9-gone", tailBlocks(), true); err == nil {
		t.Fatal("an order RDS does not hold must refuse the append")
	}
	if n := atomic.LoadInt32(&addBlocksCalls); n != 0 {
		t.Fatalf("nothing may be appended after a refusal; addBlocks called %d times", n)
	}
}

// A read that answers and names no robot is a different fact from a read that
// failed, and it gets its own words — but it is still a refusal: these blocks
// are work, and there is nobody to hand them to.
func TestReleaseOrder_RefusesWhenNoVehicleIsAssigned(t *testing.T) {
	t.Parallel()
	var addBlocksCalls int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/orderDetails/sg-9-idle":
			w.Write([]byte(`{"code":0,"msg":"ok","id":"sg-9-idle","state":"WAITING","vehicle":""}`))
		case "/addBlocks":
			atomic.AddInt32(&addBlocksCalls, 1)
			w.Write([]byte(`{"code":0,"msg":"ok"}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	adapter := New(Config{BaseURL: srv.URL, Timeout: 5 * time.Second, DebugLog: func(string, ...any) {}})

	err := adapter.ReleaseOrder("sg-9-idle", tailBlocks(), true)
	if err == nil {
		t.Fatal("an unassigned mission must refuse the append, not take an empty pin")
	}
	if !strings.Contains(err.Error(), "no assigned vehicle") {
		t.Fatalf("the refusal must name the missing robot; got %q", err)
	}
	if n := atomic.LoadInt32(&addBlocksCalls); n != 0 {
		t.Fatalf("nothing may be appended after a refusal; addBlocks called %d times", n)
	}
}

// The mark-complete call carries no blocks: it hands nobody work, so it has no
// pin to lose and no robot to insist on. It fires microseconds after /setOrder
// on the no-wait complex path, before RDS can plausibly have assigned one, so
// refusing it would shut that path down at both plants. It does not even read.
func TestReleaseOrder_MarkCompleteNeitherReadsNorRefuses(t *testing.T) {
	t.Parallel()
	var addBlocksCalls, detailCalls int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/orderDetails/"):
			atomic.AddInt32(&detailCalls, 1)
			w.WriteHeader(http.StatusInternalServerError)
		case r.URL.Path == "/addBlocks":
			atomic.AddInt32(&addBlocksCalls, 1)
			w.Write([]byte(`{"code":0,"msg":"ok"}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	adapter := New(Config{BaseURL: srv.URL, Timeout: 5 * time.Second, DebugLog: func(string, ...any) {}})

	if err := adapter.ReleaseOrder("sg-9-nowait", nil, true); err != nil {
		t.Fatalf("mark-complete must not be gated on a vehicle: %v", err)
	}
	if n := atomic.LoadInt32(&detailCalls); n != 0 {
		t.Fatalf("mark-complete has no pin to read; orderDetails called %d times", n)
	}
	if n := atomic.LoadInt32(&addBlocksCalls); n != 1 {
		t.Fatalf("mark-complete must reach the fleet; addBlocks called %d times", n)
	}
}
