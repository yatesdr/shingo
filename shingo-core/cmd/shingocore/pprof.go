package main

import (
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"strconv"
	"time"
)

// pprof.go — the Go profiler, off unless --pprof-port names a port.
//
// ITS OWN LISTENER, ON LOOPBACK ONLY. The profiler is never mounted on the
// web router: it answers on 127.0.0.1:<port> and nowhere else, so reaching it
// takes a shell on the box (curl localhost, or an SSH tunnel). Importing
// net/http/pprof also registers its routes on http.DefaultServeMux; this
// binary never serves that mux, and the listener below serves its own.
//
// A port that will not bind is logged and the profiler stays off: it is a
// diagnostic, and it must never stop Core.

// pprofAddr is the profiler's listen address for a --pprof-port value: ""
// (off) for 0, the default; loopback at that port otherwise.
func pprofAddr(port int) string {
	if port == 0 {
		return ""
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}

// startPprof serves the profiler at addr, or does nothing and returns nil
// when addr is "" (off) or will not bind.
func startPprof(addr string) *http.Server {
	if addr == "" {
		return nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("shingocore: pprof: listen %s: %v (profiler off)", addr, err)
		return nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	// No WriteTimeout: a CPU profile or trace writes for as long as it is
	// asked to run (?seconds=).
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Printf("shingocore: pprof serving on %s (loopback only)", ln.Addr())
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("shingocore: pprof: %v", err)
		}
	}()
	return srv
}
