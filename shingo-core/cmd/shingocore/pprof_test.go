package main

import (
	"net"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestPprof_NotServedWhenOff pins the profiler's off state: the flag's default
// is off, off listens nowhere, and on listens on loopback only. The only
// parseFlags call in this package: a second would redefine the flags.
func TestPprof_NotServedWhenOff(t *testing.T) {
	args := os.Args
	t.Cleanup(func() { os.Args = args })
	os.Args = []string{"shingocore"}
	if got := parseFlags().pprofPort; got != 0 {
		t.Fatalf("--pprof-port defaults to %d, want 0 (off)", got)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	at := "127.0.0.1:" + strconv.Itoa(port)

	if srv := startPprof(pprofAddr(0)); srv != nil {
		srv.Close()
		t.Fatal("startPprof served with the profiler off")
	}
	if c, err := net.DialTimeout("tcp", at, time.Second); err == nil {
		c.Close()
		t.Fatalf("something answers on %s with the profiler off", at)
	}

	if got := pprofAddr(port); got != at {
		t.Fatalf("pprofAddr(%d) = %q, want %q (loopback only)", port, got, at)
	}
	srv := startPprof(pprofAddr(port))
	if srv == nil {
		t.Fatalf("startPprof(%q) served nothing", at)
	}
	t.Cleanup(func() { srv.Close() })
	res, err := http.Get("http://" + at + "/debug/pprof/")
	if err != nil {
		t.Fatalf("GET profiler index: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("profiler index answered %d, want 200", res.StatusCode)
	}
}
