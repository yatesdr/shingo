package engine

import (
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/domain"
)

// TestCatidResolutionHint_SaysWhatEachModeDoes: the refusal's hint says what
// the process's auto-arm mode does. On auto the monitor presses CUTOVER on a
// changeover already started; it starts none (a bug fixed: the hint said "the
// automatic changeover arm will start it", the behaviour that was removed).
func TestCatidResolutionHint_SaysWhatEachModeDoes(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	pid, err := db.CreateProcess("HINT-PROC", "", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	for _, tc := range []struct{ mode, want, not string }{
		{domain.ChangeoverAutoArmAuto, "presses CUTOVER on a changeover already started", "will start it"},
		{domain.ChangeoverAutoArmPrompt, "changeover prompt on this station", ""},
		{domain.ChangeoverAutoArmOff, "start a changeover to the matching style", "CUTOVER"},
	} {
		testutil.MustNoErr(t, db.SetChangeoverAutoArm(pid, tc.mode), "set mode")
		got := eng.catidResolutionHint(pid)
		if !strings.Contains(got, tc.want) || (tc.not != "" && strings.Contains(got, tc.not)) {
			t.Errorf("mode %s: hint %q, want it to say %q and not %q", tc.mode, got, tc.want, tc.not)
		}
	}
}
