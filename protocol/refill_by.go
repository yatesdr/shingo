package protocol

// RefillBy names which robot brings an index press's back position its next
// carrier, by the role the robot plays: the stored value on the Edge's style
// claim (style_node_claims.refill_by) and in Core's plant spec. The operator
// reads it as "Robot 1" or "Robot 2" (RobotNumber); the stored words are the
// roles, so a renumbering of the screen changes no row.
//
// Core dispatch reads none of it: the Edge builds the legs from it, and Core's
// plant spec and seeder write the same words for the Edge to read.
type RefillBy string

const (
	// RefillByClearingRobot is Robot 1: the robot that lifts the spent carrier
	// off the press fetches the replacement on its way back. The default.
	RefillByClearingRobot RefillBy = "clearing_robot"
	// RefillByIndexRobot is Robot 2: the robot that indexes the on-deck
	// carrier onto the press fetches the replacement after it.
	RefillByIndexRobot RefillBy = "index_robot"
)

// RefillBys is every value, in the order the screen offers them.
func RefillBys() []RefillBy {
	return []RefillBy{RefillByClearingRobot, RefillByIndexRobot}
}

// Valid reports whether r is one of RefillBys. Blank is not a value: it is
// read as the default where a claim says nothing (OrDefault).
func (r RefillBy) Valid() bool {
	for _, v := range RefillBys() {
		if r == v {
			return true
		}
	}
	return false
}

// OrDefault is r, or the default (RefillByClearingRobot) when r is blank.
func (r RefillBy) OrDefault() RefillBy {
	if r == "" {
		return RefillByClearingRobot
	}
	return r
}

// RobotNumber is the robot the operator sees for r: 1 for the clearing robot
// (and blank), 2 for the index robot, 0 for a value it does not know.
func (r RefillBy) RobotNumber() int {
	switch r.OrDefault() {
	case RefillByClearingRobot:
		return 1
	case RefillByIndexRobot:
		return 2
	}
	return 0
}
