//go:build windows

package main

import (
	"fmt"
	"syscall"
	"time"
	"unsafe"
)

var procSystemTimes = syscall.NewLazyDLL("kernel32.dll").NewProc("GetSystemTimes")

// hostBusy is the CPU time every logical processor has spent not idle since
// boot, as gatetime's winMachine.hostBusy reads it: GetSystemTimes' kernel
// time includes its idle time.
func hostBusy() (time.Duration, error) {
	var i, k, u syscall.Filetime
	if r, _, err := procSystemTimes.Call(uintptr(unsafe.Pointer(&i)), uintptr(unsafe.Pointer(&k)), uintptr(unsafe.Pointer(&u))); r == 0 {
		return 0, fmt.Errorf("GetSystemTimes: %w", err)
	}
	ft := func(f syscall.Filetime) int64 { return int64(f.HighDateTime)<<32 | int64(f.LowDateTime) }
	return time.Duration((ft(k) + ft(u) - ft(i)) * 100), nil
}
