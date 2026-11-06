package www

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStationEditor_SuggestsTheStationKindNotTheCounterKind: the station
// counter's tag combo asks for the station kind (each station struct's count
// member by the family's convention). The counter kind puts each MES struct's
// production counter first, and a station counter may not read a member of
// the process's MES struct (stationTagFree), so its first suggestion was
// always a refusal.
func TestStationEditor_SuggestsTheStationKindNotTheCounterKind(t *testing.T) {
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
	if strings.Contains(body, `data-suggest="counter"`) || !strings.Contains(body, `data-suggest="station"`) {
		t.Errorf("the station editor's tag combo must ask for the station kind (data-suggest=\"station\"); its body:\n%s", body)
	}
}
