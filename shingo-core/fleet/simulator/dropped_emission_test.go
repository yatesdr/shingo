package simulator

import (
	"bytes"
	"errors"
	"log"
	"os"
	"strings"
	"testing"

	"shingocore/fleet"
)

type silentEmitter struct{ calls int }

func (e *silentEmitter) EmitOrderStatusChanged(int64, string, string, string, string, string, *fleet.OrderSnapshot) {
	e.calls++
}
func (e *silentEmitter) EmitBlockCompleted(int64, string, string, string, string, int64, int64) {}
func (e *silentEmitter) EmitGraceExpired(int64, string)                                         {}

type unresolvableIDs struct{}

func (unresolvableIDs) ResolveVendorOrderID(string) (int64, error) {
	return 0, errors.New("no order with that vendor id")
}

// §R.98 stage A4. A state change the fleet made and Core never heard about is
// indistinguishable, downstream, from a state change that never happened — and
// every wedge investigation on this rig begins by reading the log for what the
// fleet said. This was the one drop path that left no trace.
func TestDriveState_UnresolvableEmissionIsLogged(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	s := New()
	em := &silentEmitter{}
	s.InitTracker(em, unresolvableIDs{})
	id := mkTransport(t, s, "unresolvable")

	if old, mapped := s.DriveState(id, "RUNNING"); old != "CREATED" || mapped != "in_transit" {
		t.Fatalf("DriveState returned (%q, %q)", old, mapped)
	}
	if em.calls != 0 {
		t.Fatalf("an unresolvable vendor id must not emit; calls = %d", em.calls)
	}
	out := buf.String()
	if !strings.Contains(out, "dropped CREATED → RUNNING") || !strings.Contains(out, id) {
		t.Fatalf("the dropped emission must name the transition and the mission; got %q", out)
	}
}

// DriveStateWithRobot carries the same drop and says so the same way.
func TestDriveStateWithRobot_UnresolvableEmissionIsLogged(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	s := New()
	em := &silentEmitter{}
	s.InitTracker(em, unresolvableIDs{})
	id := mkTransport(t, s, "unresolvable-robot")

	s.DriveStateWithRobot(id, "RUNNING", "AMR-07")
	if em.calls != 0 {
		t.Fatalf("an unresolvable vendor id must not emit; calls = %d", em.calls)
	}
	out := buf.String()
	if !strings.Contains(out, "dropped CREATED → RUNNING") || !strings.Contains(out, "AMR-07") {
		t.Fatalf("the dropped emission must name the transition and the robot; got %q", out)
	}
}
