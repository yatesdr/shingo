//go:build linux

package reuse

import (
	"io/fs"
	"syscall"
	"time"
)

// changeTime is the inode's ctime: every write, chmod, rename or link count
// change moves it, and no utime call can set it back.
func changeTime(_ string, fi fs.FileInfo) (time.Time, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(st.Ctim.Sec, st.Ctim.Nsec), true
}
