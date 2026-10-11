//go:build !lockcheck && !sim

package lockcheck

import "sync"

// Mutex is sync.Mutex in a production build.
type Mutex[R Ranked] = sync.Mutex

// RWMutex is sync.RWMutex in a production build.
type RWMutex[R Ranked] = sync.RWMutex

// Enabled reports whether this build carries the guard.
const Enabled = false
