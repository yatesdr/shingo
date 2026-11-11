package fleet

import (
	"math"
	"testing"
)

// TableOnAngle is the one read that says a table turn is done, so its edges
// are pinned: the wrap at ±π, the tolerance boundary, and a table that reads
// its angle while the turn's task still runs.
func TestTableOnAngle(t *testing.T) {
	t.Parallel()
	idle := func(a float64) RobotStatus { return RobotStatus{TableAngle: a, TaskStatus: 4} }
	for _, tc := range []struct {
		name string
		r    RobotStatus
		want float64
		on   bool
	}{
		{"settled at the send", idle(1.569), 1.57, true},
		{"at rest, not turned", idle(0.0016), 1.57, false},
		{"-π is π", idle(-math.Pi + 0.01), math.Pi - 0.01, true},
		{"π reads near -π+tol", idle(math.Pi), -math.Pi + TableTurnTolerance/2, true},
		{"just inside the tolerance", idle(1.57 + TableTurnTolerance*0.99), 1.57, true},
		{"just outside the tolerance", idle(1.57 + TableTurnTolerance*1.01), 1.57, false},
		{"just outside below", idle(1.57 - TableTurnTolerance*1.01), 1.57, false},
		{"on angle while the turn still runs", RobotStatus{TableAngle: 1.569, TaskStatus: TaskStatusRunning}, 1.57, false},
		{"a full turn away is the same angle", idle(1.57 + 2*math.Pi), 1.57, true},
	} {
		if got := TableOnAngle(tc.r, tc.want); got != tc.on {
			t.Errorf("%s: TableOnAngle(angle %.4f, task %d; want %.4f) = %v, want %v",
				tc.name, tc.r.TableAngle, tc.r.TaskStatus, tc.want, got, tc.on)
		}
	}
}
