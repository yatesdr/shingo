//go:build windows

package reuse

import (
	"io/fs"
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32                   = syscall.NewLazyDLL("kernel32.dll")
	procGetFileInformationByEx = kernel32.NewProc("GetFileInformationByHandleEx")
)

// fileBasicInfo is FILE_BASIC_INFO.
type fileBasicInfo struct {
	CreationTime, LastAccessTime, LastWriteTime, ChangeTime int64
	FileAttributes                                          uint32
	_                                                       uint32
}

// changeTime is NTFS's ChangeTime (FILE_BASIC_INFO): moved by every write and
// metadata change. Unlike Linux's ctime a program can set it, but nothing the
// gate runs does.
func changeTime(p string, _ fs.FileInfo) (time.Time, bool) {
	name, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return time.Time{}, false
	}
	const fileReadAttributes = 0x80
	const backupSemantics = 0x02000000 // needed to open a directory
	const openReparsePoint = 0x00200000
	h, err := syscall.CreateFile(name, fileReadAttributes,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, backupSemantics|openReparsePoint, 0)
	if err != nil {
		return time.Time{}, false
	}
	defer syscall.CloseHandle(h) // read-only handle
	var info fileBasicInfo
	r, _, _ := procGetFileInformationByEx.Call(uintptr(h), 0 /* FileBasicInfo */, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
	if r == 0 {
		return time.Time{}, false
	}
	// 100 ns ticks since 1601-01-01 UTC.
	ft := syscall.Filetime{LowDateTime: uint32(info.ChangeTime), HighDateTime: uint32(info.ChangeTime >> 32)}
	return time.Unix(0, ft.Nanoseconds()), true
}

// birthTime is NTFS's CreationTime.
func birthTime(_ string, fi fs.FileInfo) (time.Time, bool) {
	d, ok := fi.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(0, d.CreationTime.Nanoseconds()), true
}
