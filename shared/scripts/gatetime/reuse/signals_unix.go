//go:build !windows

package reuse

import (
	"os"
	"syscall"
)

// forwarded are the signals passed on to the binary (forwardSignals).
var forwarded = []os.Signal{syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP}
