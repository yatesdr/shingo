package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// CountExample is one worked example of testdata/count-examples.json: a
// payload's lines, a bin's count, and what count.go answers for them.
type countExample struct {
	Name       string         `json:"name"`
	Lines      []ManifestItem `json:"lines"`
	Count      int            `json:"count"`
	Ratio      int            `json:"ratio"`
	UOP        int            `json:"uop"`
	Parts      int            `json:"parts"`
	LinePieces []int64        `json:"line_pieces"`
}

func readCountExamples(t *testing.T) []countExample {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "count-examples.json"))
	if err != nil {
		t.Fatalf("read the worked examples: %v", err)
	}
	var f struct {
		Examples []countExample `json:"examples"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parse the worked examples: %v", err)
	}
	if len(f.Examples) == 0 {
		t.Fatal("testdata/count-examples.json holds no examples: nothing was checked")
	}
	return f.Examples
}

// TestCountExamples: UOPRatio, SplitUOP and LinePieces answer every worked
// example (the owner's 800 at 8, 803, 404, 403, -5, 1500 at 5, the 60/60/120
// kit, ratio 1), and the split never loses a part: uop*ratio + parts == count.
func TestCountExamples(t *testing.T) {
	t.Parallel()
	for _, ex := range readCountExamples(t) {
		if got := UOPRatio(ex.Lines); got != ex.Ratio {
			t.Errorf("%s: UOPRatio = %d, want %d", ex.Name, got, ex.Ratio)
		}
		uop, parts := SplitUOP(ex.Count, ex.Ratio)
		if uop != ex.UOP || parts != ex.Parts {
			t.Errorf("%s: SplitUOP(%d, %d) = %d UOP, %d parts; want %d, %d", ex.Name, ex.Count, ex.Ratio, uop, parts, ex.UOP, ex.Parts)
		}
		if uop*max(ex.Ratio, 1)+parts != ex.Count {
			t.Errorf("%s: %d UOP and %d parts at %d is not the count %d", ex.Name, uop, parts, ex.Ratio, ex.Count)
		}
		if len(ex.LinePieces) != len(ex.Lines) {
			t.Fatalf("%s: the example gives %d line pieces for %d lines", ex.Name, len(ex.LinePieces), len(ex.Lines))
		}
		for i, want := range ex.LinePieces {
			if got := LinePieces(ex.Lines, i, int64(ex.Count)); got != want {
				t.Errorf("%s: LinePieces(line %d) = %d, want %d", ex.Name, i, got, want)
			}
		}
		if got := LinePieces(ex.Lines, len(ex.Lines), int64(ex.Count)); got != 0 {
			t.Errorf("%s: a line past the template = %d, want 0", ex.Name, got)
		}
	}
}
