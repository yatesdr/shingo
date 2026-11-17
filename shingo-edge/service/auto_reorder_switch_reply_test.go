package service

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/internal/testdb"
	"shingoedge/store/processes"
)

// TestAutoReorderSwitch_TheReplyIsWhatProcessesReads pins GET
// /api/processes/{id}/auto-reorder's body (ProcessService.AutoReorderSwitch)
// to what Settings › Auto-reorder reads (processes-desktop.js
// autoReorderSection): how many of the line's positions a person switched off,
// of how many, and the last flip. The station setting is the process row's
// (auto_reorder_switch_hmi), read with the row.
func TestAutoReorderSwitch_TheReplyIsWhatProcessesReads(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	pid, err := db.CreateProcess("SWITCH-REPLY", "", "PLC1", "COUNT", true)
	testutil.MustNoErr(t, err, "create process")
	for i, on := range []bool{false, true, false} {
		_, err := processes.FlipAutoReorder(db.DB, pid, on, "Pat", domain.FlipFromProcesses, time.Now().Add(time.Duration(i)*time.Minute))
		testutil.MustNoErr(t, err, "flip")
	}
	sw, err := NewProcessService(db).AutoReorderSwitch(pid)
	testutil.MustNoErr(t, err, "read the switch")
	raw, err := json.Marshal(sw)
	testutil.MustNoErr(t, err, "encode")
	var body map[string]json.RawMessage
	testutil.MustNoErr(t, json.Unmarshal(raw, &body), "decode")
	var keys []string
	for k := range body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// Before the base moved it also carried the station setting again
	// (shown_on_station) and ten flips, of which the page reads the first.
	if got := strings.Join(keys, ","); got != "last_flip,positions,switched_off" {
		t.Errorf("the reply carries %s, want last_flip,positions,switched_off", got)
	}
	if sw.LastFlip == nil || sw.LastFlip.TurnedOn || sw.LastFlip.By != "Pat" {
		t.Errorf("last flip = %+v, want the newest, an off by Pat", sw.LastFlip)
	}
	never, err := db.CreateProcess("SWITCH-NEVER", "", "PLC2", "COUNT", true)
	testutil.MustNoErr(t, err, "a line never flipped")
	sw, err = NewProcessService(db).AutoReorderSwitch(never)
	testutil.MustNoErr(t, err, "read its switch")
	if sw.LastFlip != nil {
		t.Errorf("a line never flipped has last flip %+v, want none", sw.LastFlip)
	}
}
