package domain

import (
	"shingo/protocol/testutil"
	"testing"
)

func TestBinStatus_ScanValue_Roundtrip(t *testing.T) {
	t.Parallel()
	for _, original := range AllBinStatuses() {
		v, err := original.Value()
		if err != nil {
			t.Fatalf("%s.Value() error: %v", original, err)
		}
		var got BinStatus
		if err := got.Scan(v); err != nil {
			t.Fatalf("%s: Scan error: %v", original, err)
		}
		if got != original {
			t.Errorf("roundtrip: got %q, want %q", got, original)
		}
	}
}

func TestBinStatus_Scan_NullEmpty(t *testing.T) {
	t.Parallel()
	var s BinStatus
	testutil.MustNoErr(t, s.Scan(nil), "Scan(nil) error")
	if s != "" {
		t.Errorf("Scan(nil) -> %q, want empty", s)
	}
	testutil.MustNoErr(t, s.Scan([]byte("staged")), "Scan([]byte) error")
	if s != BinStatusStaged {
		t.Errorf("Scan([]byte staged) -> %q, want %q", s, BinStatusStaged)
	}
}
