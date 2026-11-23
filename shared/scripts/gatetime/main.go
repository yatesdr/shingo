// Command gatetime is the gate's timing instrument and its test-binary
// recorder, one program so that one job-object implementation serves both.
//
//	gatetime exec BINARY ARGS...   go test -exec's program: runs one test
//	                               binary in a job object of its own and, when
//	                               $GATETIME_MANIFESTS is set, writes one JSON
//	                               manifest of what that run read (exec.go)
//
// The Windows calls (job objects) are behind //go:build windows; on any other
// OS the same commands build and run with what that OS can give (proc_other.go
// says what is missing there).
package main

import (
	"fmt"
	"os"
)

// commands are the subcommands, by name. Each returns the process exit code.
var commands = map[string]func(args []string) int{
	"exec": execMain,
}

func usage() int {
	fmt.Fprintln(os.Stderr, "usage: gatetime exec BINARY ARGS...")
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
