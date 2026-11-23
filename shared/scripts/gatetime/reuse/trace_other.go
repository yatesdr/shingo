//go:build !(linux && amd64)

package reuse

import (
	"fmt"
	"os"
)

// ShimCommand is the subcommand the Linux tracer starts the wrapper with. It
// exists here so a program's command table builds on every OS.
const ShimCommand = "seccomp-shim"

// ShimMain is the Linux tracer's shim; there is no tracer on this OS.
func ShimMain([]string) int {
	fmt.Fprintln(os.Stderr, "gatetime: the seccomp shim runs on linux/amd64 only")
	return 2
}

// DefaultRunner off linux/amd64 is an untraced run: children unknown, so every
// binary is keyed on the whole tree, unless the caller passes a runner that
// counts children (the timing lane's Windows job object).
func DefaultRunner(spec RunSpec) RunResult { return plainRun(spec) }
