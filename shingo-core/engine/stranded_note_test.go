package engine

import (
	"strings"
	"testing"

	"shingocore/fleet"
)

// A RESTING JACK READS A HAIR BELOW ZERO. %.4f renders -0.00001 as "-0.0000",
// and HK bin 13's log line alternated between that and "0.0000" on successive
// ticks, re-announcing one stranding every few minutes.
func TestStrandedDetail_NegativeZeroHeightPrintsAsZero(t *testing.T) {
	got := strandedDetail(fleet.RobotStatus{JackState: 3, LiftHeight: -0.00001})
	if !strings.Contains(got, "height=0.0000") || strings.Contains(got, "-0.0000") {
		t.Errorf("detail = %q, want height=0.0000", got)
	}
}

// THE NOTE IS THE SHORT FORM. Reason, robot, point and x/y reach the bins page;
// angle and the jack reading go to the log line only.
func TestStrandedNote_IsShort(t *testing.T) {
	r := fleet.RobotStatus{CurrentStation: "PP192", LastStation: "LM48", X: 1.06, Y: 29.58,
		Angle: 3.14, JackState: 3, LiftHeight: -0.0001}
	got := strandedNote("AMR-12", r, true, "robot not at a known node")
	if want := "robot not at a known node; AMR-12 at PP192 (x=1.06 y=29.58)"; got != want {
		t.Errorf("note = %q, want %q", got, want)
	}
}
