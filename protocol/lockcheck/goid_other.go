//go:build (lockcheck || sim) && !(gc && (amd64 || arm64))

package lockcheck

// canReadG says this build reads the running goroutine's g directly. It has no
// assembly for this architecture or compiler, so goid reads runtime.Stack.
const canReadG = false

// gWord is never called when canReadG is false.
func gWord(uintptr) uint64 { return 0 }
