//go:build (lockcheck || sim) && gc && (amd64 || arm64)

package lockcheck

// canReadG says this build reads the running goroutine's g directly
// (goid_amd64.s, goid_arm64.s).
const canReadG = true

// gWord is the 8-byte word off bytes into the calling goroutine's g, the
// runtime's record of it. off must be inside the g (calibrate's gSpan).
func gWord(off uintptr) uint64
