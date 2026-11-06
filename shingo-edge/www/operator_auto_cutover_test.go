package www

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"shingoedge/domain"
	"shingoedge/engine"
)

// TestCATIDAutoCutoverEvent_FieldsAreRead: the station's automatic-cutover
// notice reads ChangeoverID, NewCATID, Forced and Blockers off the event as
// the SSE arm sends it (the Go struct, marshalled here), so a field renamed on
// either side fails.
func TestCATIDAutoCutoverEvent_FieldsAreRead(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping the auto-cutover notice test")
	}
	events := map[string]engine.CATIDAutoCutoverEvent{
		"gated": {ProcessID: 3, ProcessName: "CELL-A", ChangeoverID: 41, NewCATID: "1002"},
		"forced": {ProcessID: 3, ProcessName: "CELL-A", ChangeoverID: 41, NewCATID: "1002", Forced: true,
			Blockers: []domain.Blocker{{Reason: "task at node N1 in staging_requested"}, {Reason: "order 7 in staged"}}},
	}
	b, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(t.TempDir(), "events.json")
	if err := os.WriteFile(f, b, 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join("static", "operator-station", "operator-auto-cutover.test.js")
	out, err := exec.Command(nodePath, script, f).CombinedOutput()
	if err != nil {
		t.Fatalf("auto-cutover notice test failed:\n%s\nerror: %v", out, err)
	}
}
