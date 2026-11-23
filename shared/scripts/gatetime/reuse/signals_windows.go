//go:build windows

package reuse

import "os"

// forwarded is empty on Windows: os.Process.Signal can only kill there, so
// there is nothing to pass on (go test's timeout kills the wrapper, and the
// timing lane's job object takes the binary with it).
var forwarded []os.Signal
