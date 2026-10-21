package www

import (
	"encoding/json"
	"os"
	"regexp"
	"testing"
)

// TestChangeoverUpdate_NamesItsProcess holds every changeover-update the
// station handlers broadcast to naming the process it happened on: a station
// refetches on it only when it draws that process (operator-state.js
// eventConcerns), so an emit site that names none would refresh every station
// on the Edge. The start broadcasts the changeover row, which carries
// process_id; every other site goes through changeoverAction.
func TestChangeoverUpdate_NamesItsProcess(t *testing.T) {
	src, err := os.ReadFile("handlers_operator_changeover.go")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	sites := regexp.MustCompile(`SSEEvent\{Type: "changeover-update", Data: ([^}]*\))?([^}]*)\}`).FindAllStringSubmatch(string(src), -1)
	if len(sites) < 10 {
		t.Fatalf("found %d changeover-update emit sites; the pattern has stopped seeing them", len(sites))
	}
	for _, s := range sites {
		data := s[1] + s[2]
		if data != "co" && !regexp.MustCompile(`^changeoverAction\("[a-z-]+", processID\)$`).MatchString(data) {
			t.Errorf("a changeover-update carries %q, which names no process", data)
		}
	}
	raw, err := json.Marshal(changeoverAction("release", 42))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) != `{"action":"release","process_id":42}` {
		t.Errorf("changeoverAction = %s", raw)
	}
}
