package www

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStationEditor_SuggestsEveryTagNotTheCounterKind: the station counter's
// tag combo asks for no kind. The counter kind puts each MES struct's
// production counter first, and a station counter may not read a tag a
// production counter reads (stationTagFree), so its first suggestion was
// always a refusal.
func TestStationEditor_SuggestsEveryTagNotTheCounterKind(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("static", "js", "pages", "processes-desktop.js"))
	if err != nil {
		t.Fatalf("read processes-desktop.js: %v", err)
	}
	s := strings.ReplaceAll(string(src), "\r\n", "\n")
	start := strings.Index(s, "\nfunction openStationCounter(")
	if start < 0 {
		t.Fatal("openStationCounter not found in processes-desktop.js")
	}
	body := s[start+1:]
	if end := strings.Index(body, "\n}\n"); end >= 0 {
		body = body[:end]
	}
	if strings.Contains(body, `data-suggest="counter"`) || !strings.Contains(body, `data-suggest=""`) {
		t.Errorf("the station editor's tag combo must ask for no kind (data-suggest=\"\"); its body:\n%s", body)
	}
}
