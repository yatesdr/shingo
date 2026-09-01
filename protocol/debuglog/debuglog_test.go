package debuglog

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The debug file must be opened O_APPEND. logrotate's copytruncate copies then
// truncates the path without touching the process's fd: truncation does not
// reset the fd's offset, so a non-append fd keeps writing at the pre-rotation
// offset — the file instantiates a hole of NUL bytes up to that offset, the
// apparent size never returns to ~0, and maxsize/notifempty stop meaning
// anything. Verified on Linux: without O_APPEND the post-truncate write lands
// at the stale offset; with O_APPEND it lands at 0.
func TestOpenUsesAppendMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows caches the append handle's end-of-file separately from a
		// truncate issued through another handle, so this path-truncate
		// probe does not isolate the O_APPEND contract there. copytruncate
		// is Linux-only; CI's Linux test runs enforce this test.
		t.Skip("truncate-via-second-handle probe is Linux-only (copytruncate contract)")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "shingo-debug.log")
	t.Chdir(dir)

	l, err := New(8, []string{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { l.Close() })

	// Write enough to establish an offset, then simulate copytruncate's
	// copy-then-truncate of the path.
	l.Log("sub", "line-%d", 1)
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) == 0 {
		t.Fatal("debug file empty after Log")
	}
	if err := os.Truncate(target, 0); err != nil {
		t.Fatal(err)
	}

	// Post-truncate writes must land at offset 0. A non-append fd would
	// produce a NUL-prefixed hole here.
	l.Log("sub", "after-truncate")
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(string(after), "\x00") {
		t.Fatalf("file contains NUL hole after copytruncate-style truncate; got %q", after)
	}
	// The file sink prefixes each line with a timestamp ("... [sub] msg"), so
	// "landed at offset 0" shows up as the marker being the LAST text in the
	// file — a stale-offset write would leave the marker after the hole.
	if !strings.HasSuffix(string(after), "after-truncate\n") {
		t.Fatalf("post-truncate write did not land at offset 0; got %q", after)
	}
}
