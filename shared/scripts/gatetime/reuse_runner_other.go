//go:build !windows

package main

import "shingo/shared/scripts/gatetime/reuse"

// reuseRunner off Windows is reuse's own: the ptrace tracer on linux/amd64,
// an untraced run (children unknown) elsewhere.
var reuseRunner reuse.Runner
