//go:build !linux && !windows

package reuse

import (
	"io/fs"
	"time"
)

// changeTime off Linux and Windows is the mtime: changedSince already reads it,
// so this adds nothing, and says so.
func changeTime(_ string, fi fs.FileInfo) (time.Time, bool) {
	return fi.ModTime(), true
}
