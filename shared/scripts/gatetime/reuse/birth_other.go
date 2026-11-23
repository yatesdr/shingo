//go:build !(linux && amd64) && !windows

package reuse

import (
	"io/fs"
	"time"
)

// birthTime is unknown here: changedSince falls back to the change time.
func birthTime(string, fs.FileInfo) (time.Time, bool) { return time.Time{}, false }
