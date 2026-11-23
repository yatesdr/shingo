//go:build linux && amd64

package reuse

import (
	"encoding/binary"
	"io/fs"
	"syscall"
	"time"
	"unsafe"
)

// birthTime is the inode's creation time from statx (STATX_BTIME), when the
// file system reports one (ext4, xfs, btrfs, tmpfs and overlayfs on the
// houseserver's 6.12 kernel all do).
func birthTime(p string, _ fs.FileInfo) (time.Time, bool) {
	const sysStatx, atSymlinkNofollow, statxBtime = 332, 0x100, 0x800
	name, err := syscall.BytePtrFromString(p)
	if err != nil {
		return time.Time{}, false
	}
	atFdcwd := -100
	var buf [256]byte // struct statx
	_, _, e := syscall.Syscall6(sysStatx, uintptr(atFdcwd), uintptr(unsafe.Pointer(name)), atSymlinkNofollow,
		statxBtime, uintptr(unsafe.Pointer(&buf[0])), 0)
	if e != 0 || binary.LittleEndian.Uint32(buf[0:4])&statxBtime == 0 {
		return time.Time{}, false
	}
	sec := int64(binary.LittleEndian.Uint64(buf[80:88])) // stx_btime.tv_sec
	nsec := int64(binary.LittleEndian.Uint32(buf[88:92]))
	return time.Unix(sec, nsec), true
}
