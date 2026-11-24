//go:build !windows

package main

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

// hostBusy reads /proc/stat's first line (user nice system idle iowait irq
// softirq steal ...), in USER_HZ ticks, as gatetime's otherMachine.hostBusy
// reads it: busy is all but idle and iowait. Under WSL 2 it is the utility
// VM's CPU, every distro and Docker Desktop's containers in it; the Windows
// host's own processes are not in it.
func hostBusy() (time.Duration, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, err
	}
	line, _, _ := strings.Cut(string(data), "\n")
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "cpu" {
		return 0, errors.New("/proc/stat: no cpu line")
	}
	var busy int64
	for i, v := range f[1:] {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, err
		}
		if i != 3 && i != 4 && i < 8 { // not idle, not iowait, not guest (counted in user)
			busy += n
		}
	}
	return time.Duration(busy) * (time.Second / 100), nil
}
