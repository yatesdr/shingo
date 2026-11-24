// Command gatetime is the gate's timing instrument and its test-binary
// recorder, one program so that one job-object implementation serves both.
//
//	gatetime run -out DIR [-every 3s] [-repo DIR] -- cmd args...
//	                               times one command in a job object, holding
//	                               the timing flag and the standby requirement,
//	                               and judges the machine it ran on (run.go)
//	gatetime compare A1 B1 B2 A2   judges one A B B A pair of run directories
//	                               (compare.go)
//	gatetime stamp -since UNIX     the load stamps gate.sh puts on its steps
//	                               log's total line (stamp.go)
//	gatetime report DIR...         what the manifest directories show: children,
//	                               reads outside the module, the implied hit
//	                               rate (report.go)
//	gatetime exec BINARY ARGS...   go test -exec's program: runs one test
//	                               binary in a job object of its own and, when
//	                               $GATETIME_MANIFESTS is set, writes one JSON
//	                               manifest of what that run read (exec.go).
//	                               With $GATETIME_REUSE set it is package
//	                               reuse's instead (record, reuse or audit)
//	gatetime canary -root DIR      reuse's canary: run its known binary
//	                               through `exec` and check the record
//	gatetime seccomp-shim ...      the Linux tracer's shim (not for hands)
//	gatetime host                  the Windows host's readings for a gatetime
//	                               inside WSL, one JSON request a line (host.go;
//	                               not for hands)
//
// The Windows calls (job objects, the process table, power, the standby log)
// are behind //go:build windows; on any other OS the same commands build and
// run with what that OS can give (job_other.go and sys_other.go say what is
// missing there), and inside WSL `run` and `stamp` read the Windows host
// through the Windows build's `host` (host.go).
package main

import (
	"fmt"
	"os"

	"shingo/shared/scripts/gatetime/reuse"
)

// commands are the subcommands, by name. Each returns the process exit code.
var commands = map[string]func(args []string) int{
	"exec":            execMain,
	"run":             runMain,
	"compare":         compareMain,
	"stamp":           stampMain,
	"report":          reportMain,
	"canary":          reuse.CanaryMain,
	"host":            hostMain,
	reuse.ShimCommand: reuse.ShimMain,
}

func usage() int {
	fmt.Fprintln(os.Stderr, "usage: gatetime run -out DIR [-every 3s] [-repo DIR] -- cmd args...")
	fmt.Fprintln(os.Stderr, "       gatetime compare [-a NAME -b NAME] A1 B1 B2 A2")
	fmt.Fprintln(os.Stderr, "       gatetime stamp -since UNIX")
	fmt.Fprintln(os.Stderr, "       gatetime report MANIFEST_DIR...")
	fmt.Fprintln(os.Stderr, "       gatetime exec BINARY ARGS...")
	fmt.Fprintln(os.Stderr, "       gatetime canary -root DIR [-identity FILE]")
	return 2
}

func main() {
	if len(os.Args) < 2 {
		os.Exit(usage())
	}
	run, ok := commands[os.Args[1]]
	if !ok {
		os.Exit(usage())
	}
	os.Exit(run(os.Args[2:]))
}
