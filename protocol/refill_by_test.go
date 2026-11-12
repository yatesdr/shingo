package protocol

import "testing"

// TestRefillBy_TheThreeRobots: the stored words are the roles, the screen's
// numbers are 1, 2 and 3 in that order, blank is Robot 1, and a word that is
// no robot is not valid and numbers 0.
func TestRefillBy_TheThreeRobots(t *testing.T) {
	t.Parallel()
	want := map[RefillBy]int{RefillByClearingRobot: 1, RefillByIndexRobot: 2, RefillByOwnRobot: 3}
	for i, r := range RefillBys() {
		if !r.Valid() || r.RobotNumber() != i+1 || want[r] != i+1 {
			t.Errorf("RefillBys()[%d] = %q: valid %v, Robot %d; want valid, Robot %d", i, r, r.Valid(), r.RobotNumber(), i+1)
		}
	}
	if len(RefillBys()) != len(want) {
		t.Errorf("RefillBys() = %v, want the three robots", RefillBys())
	}
	if blank := RefillBy(""); blank.Valid() || blank.OrDefault() != RefillByClearingRobot || blank.RobotNumber() != 1 {
		t.Errorf("blank: valid %v, default %q, Robot %d; want not valid, clearing_robot, Robot 1", blank.Valid(), blank.OrDefault(), blank.RobotNumber())
	}
	if odd := RefillBy("robot_9"); odd.Valid() || odd.RobotNumber() != 0 {
		t.Errorf("robot_9: valid %v, Robot %d; want not valid, 0", odd.Valid(), odd.RobotNumber())
	}
}
