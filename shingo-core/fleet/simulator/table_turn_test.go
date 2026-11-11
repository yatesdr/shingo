package simulator

import (
	"testing"
	"time"

	"shingo/protocol/clock"
	"shingocore/fleet"
)

// The simulated table behaves as the field test measured a SEER table: a turn
// reads task_status 2 until it settles, then the angle; a refused turn and a
// turn that never moves are switchable, and every send is recorded.
func TestSimTable_TurnSettlesAfterItsDelay(t *testing.T) {
	t.Parallel()
	clk := clock.NewManual(time.Date(2026, 11, 10, 14, 50, 43, 0, time.UTC))
	s := New(WithClock(clk))
	s.SetTableTurnDelay(8 * time.Second)

	if err := s.TurnTable("AMR-05", 1.57); err != nil {
		t.Fatalf("turn: %v", err)
	}
	if st := s.TableStatus("AMR-05"); st.TaskStatus != fleet.TaskStatusRunning || fleet.TableOnAngle(st, 1.57) {
		t.Errorf("mid-turn: %+v, want the task running and not on angle", st)
	}
	clk.Advance(8 * time.Second)
	if st := s.TableStatus("AMR-05"); !fleet.TableOnAngle(st, 1.57) || st.JackState != 3 {
		t.Errorf("settled: angle %.3f task %d jack %d, want on angle with the jack down", st.TableAngle, st.TaskStatus, st.JackState)
	}

	s.SetTableNeverTurns(true)
	if err := s.TurnTable("AMR-05", 0); err != nil {
		t.Fatalf("a turn the robot acks: %v", err)
	}
	clk.Advance(time.Minute)
	if st := s.TableStatus("AMR-05"); !fleet.TableOnAngle(st, 1.57) {
		t.Errorf("a table that never moves read %.3f, want it left at 1.57", st.TableAngle)
	}
	s.SetTableNeverTurns(false)
	s.SetTableTurnFails(true)
	if err := s.TurnTable("AMR-05", 0); err == nil {
		t.Error("a refused turn returned no error")
	}
	if n := len(s.TableTurnCalls()); n != 3 {
		t.Errorf("%d sends recorded, want all 3", n)
	}
	s.SetRobotJackUp("AMR-05", true)
	if st := s.TableStatus("AMR-05"); st.JackState != 1 || !st.JackIsFull {
		t.Errorf("jack up: %+v, want state 1 and full", st)
	}
}
