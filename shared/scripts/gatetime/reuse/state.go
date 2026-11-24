package reuse

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// The ways a run touched a path, weakest first. A path touched more than one
// way keeps the strongest (merge).
const (
	OpStat = "stat" // statted, accessed, readlink'd, created, removed, renamed
	OpExec = "exec" // executed
	OpOpen = "open" // opened (a directory opened is a directory listed)
)

func opRank(op string) int {
	switch op {
	case OpOpen:
		return 2
	case OpExec:
		return 1
	}
	return 0
}

// PathRec is one path a run touched and what it was when the run ended.
type PathRec struct {
	Path string `json:"path"`
	Op   string `json:"op"`
	// Child is true when only a child process touched it (the Linux trace).
	Child bool `json:"child,omitempty"`
	// State is the path's content as stateOf spells it.
	State string `json:"state"`
}

// errUnhashable is a path whose content the wrapper cannot state: a run that
// read one is never stored.
var errUnhashable = errors.New("unhashable")

// stateOf is a path's content now, by content and never by mtime:
//
//	missing                     nothing there
//	file:<perm>:<sha256>        a regular file (open, stat or exec alike)
//	dir:<sha256 of listing>     a directory that was opened (listed)
//	dir                         a directory that was only statted
//	special:<type>              a device, pipe or socket that was only statted
//	link:<target>|<state>       a symlink: its target and what it leads to
//
// A special file that was opened has no content to state: errUnhashable.
func stateOf(p, op string) (string, error) {
	fi, err := os.Lstat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "missing", nil
	case err != nil:
		return "", fmt.Errorf("%w: %v", errUnhashable, err)
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		target, err := os.Readlink(p)
		if err != nil {
			return "", fmt.Errorf("%w: %v", errUnhashable, err)
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(p), target)
		}
		// A loop or a long chain ends in an error from Stat, which is unhashable.
		if _, err := os.Stat(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("%w: %v", errUnhashable, err)
		}
		rest, err := stateOf(target, op)
		if err != nil {
			return "", err
		}
		return "link:" + target + "|" + rest, nil
	}
	return statState(p, op, fi)
}

func statState(p, op string, fi fs.FileInfo) (string, error) {
	switch {
	case fi.Mode().IsRegular():
		s, err := sha256File(p)
		if err != nil {
			return "", fmt.Errorf("%w: %v", errUnhashable, err)
		}
		return fmt.Sprintf("file:%o:%s", fi.Mode().Perm(), s), nil
	case fi.IsDir() && op == OpOpen:
		return listingState(p)
	case fi.IsDir():
		return "dir", nil
	case op == OpOpen || op == OpExec:
		return "", fmt.Errorf("%w: %s is a %s", errUnhashable, p, fi.Mode().Type())
	default:
		return "special:" + fi.Mode().Type().String(), nil
	}
}

// listingState is a directory's entries: names and types, sorted.
func listingState(p string) (string, error) {
	ents, err := os.ReadDir(p)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errUnhashable, err)
	}
	lines := make([]string, 0, len(ents))
	for _, e := range ents {
		lines = append(lines, e.Name()+"\x00"+e.Type().String())
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return "dir:" + hex.EncodeToString(sum[:]), nil
}

// changedSince is whether what stateOf(p, op) states may have changed at or
// after t less 2 s (Go's own test-cache slack for coarse file-system clocks).
// It only ever stops a run being STORED: a hit is decided by content
// (stateOf), never by a time.
//
// For content (a file, a listed directory, a link) it reads the change time
// (ctime on Linux, ChangeTime on Windows), which `touch -r` cannot set back,
// and the mtime. A directory that was only statted is stated as "dir": its
// entries are not part of it, and its ctime moves whenever one is added (/tmp
// moves all the time), so what could falsify it is the directory being made
// or replaced during the run: its birth time. Where no birth time is reported
// the change time stands in (it moves on creation too: fail safe).
func changedSince(p, op string, t time.Time) bool {
	fi, err := os.Lstat(p)
	if err != nil {
		return false // missing now: its state says so, and is compared as such
	}
	edge := t.Add(-2 * time.Second)
	if fi.IsDir() && op != OpOpen {
		if bt, ok := birthTime(p, fi); ok {
			return !bt.Before(edge)
		}
	}
	if !fi.ModTime().Before(edge) {
		return true
	}
	ct, ok := changeTime(p, fi)
	return !ok || !ct.Before(edge)
}

// exclusions are the paths a run touches that are not inputs to its verdict.
type exclusions struct {
	exact    map[string]bool
	prefixes []string // each ends in a separator
}

// newExclusions: the run's own work directory (its private temp dir and the
// test log), the binary and the wrapper (both already in the key), and the
// machine's state: /proc, /sys and the stateless devices. Machine state is not
// an input for the same reason the clock and the load are not (citrine's
// inputs table, last row): cert and GitHub run everything.
func newExclusions(work string, files ...string) *exclusions {
	x := &exclusions{exact: map[string]bool{}}
	for _, f := range files {
		if f != "" {
			x.exact[filepath.Clean(f)] = true
		}
	}
	if work != "" {
		x.addPrefix(work)
		if long, err := filepath.EvalSymlinks(work); err == nil {
			x.addPrefix(long)
		}
	}
	if runtime.GOOS == "windows" {
		for _, d := range []string{"NUL", "CON", `\\.\NUL`} {
			x.exact[d] = true
		}
		x.prefixes = append(x.prefixes, `\\.\`)
		return x
	}
	for _, d := range []string{"/dev/null", "/dev/zero", "/dev/full", "/dev/random", "/dev/urandom", "/dev/tty",
		"/dev/ptmx", "/dev/stdin", "/dev/stdout", "/dev/stderr", "/proc", "/sys", "/dev/pts", "/dev/fd"} {
		x.exact[d] = true
	}
	x.prefixes = append(x.prefixes, "/proc/", "/sys/", "/dev/pts/", "/dev/fd/")
	return x
}

func (x *exclusions) addPrefix(dir string) {
	dir = filepath.Clean(dir)
	x.exact[dir] = true
	x.prefixes = append(x.prefixes, dir+string(filepath.Separator))
}

func (x *exclusions) has(p string) bool {
	if x.exact[p] || (runtime.GOOS == "windows" && winNullDevice(p)) {
		return true
	}
	for _, pre := range x.prefixes {
		if strings.HasPrefix(p, pre) || (runtime.GOOS == "windows" && strings.HasPrefix(strings.ToLower(p), strings.ToLower(pre))) {
			return true
		}
	}
	return false
}

// winNullDevice is whether Win32 opens p as the null device: a final element
// NUL, in any case, in any directory. A child started with a nil Stdin or
// Stdout makes package os open "NUL", which the test log parser resolves
// against the cwd, so this is <cwd>\NUL far more often than the bare name.
// Narrower than Win32 on purpose (pin:
// TestExclusions_TheNullDeviceIsNoInputInAnyDirectory): NUL.txt (a file on
// Windows 11), "NUL " and "NUL:" stay inputs, and a `\\?\` path skips Win32's
// device names, so it can be a real file. A device left out is stated as one
// and the run is refused, never stored; a file wrongly left out would be a
// false green.
func winNullDevice(p string) bool {
	if strings.HasPrefix(p, `\\?\`) {
		return false
	}
	return strings.EqualFold(p[strings.LastIndexAny(p, `\/:`)+1:], "NUL")
}

// pathSet gathers touched paths, keeping each one's strongest op.
type pathSet map[string]PathRec

func (s pathSet) add(p, op string, child bool) {
	if p == "" {
		return
	}
	p = filepath.Clean(p)
	old, ok := s[p]
	if !ok {
		s[p] = PathRec{Path: p, Op: op, Child: child}
		return
	}
	if opRank(op) > opRank(old.Op) {
		old.Op = op
	}
	old.Child = old.Child && child
	s[p] = old
}

// recs is the set, excluded paths left out, sorted by path.
func (s pathSet) recs(x *exclusions) []PathRec {
	out := make([]PathRec, 0, len(s))
	for p, r := range s {
		if !x.has(p) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
