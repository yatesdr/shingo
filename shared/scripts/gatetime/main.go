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
//	gatetime exec BINARY ARGS...   go test -exec's program: runs one test
//	                               binary in a job object of its own and, when
//	                               $GATETIME_MANIFESTS is set, writes one JSON
//	                               manifest of what that run read (exec.go)
//
// The Windows calls (job objects, the process table, power, the standby log)
// are behind //go:build windows; on any other OS the same commands build and
// run with what that OS can give (job_other.go and sys_other.go say what is
// missing there).
package main

import (
	"fmt"
	"os"
)

// commands are the subcommands, by name. Each returns the process exit code.
var commands = map[string]func(args []string) int{
	"exec":    execMain,
	"run":     runMain,
	"compare": compareMain,
	"stamp":   stampMain,
}

func usage() int {
	fmt.Fprintln(os.Stderr, "usage: gatetime run -out DIR [-every 3s] [-repo DIR] -- cmd args...")
	fmt.Fprintln(os.Stderr, "       gatetime compare [-a NAME -b NAME] A1 B1 B2 A2")
	fmt.Fprintln(os.Stderr, "       gatetime stamp -since UNIX")
	fmt.Fprintln(os.Stderr, "       gatetime exec BINARY ARGS...")
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
