package clock

import "time"

// FreezeWall pins s's wall clock at at, so an external test reads two clocks at
// exactly the same real instant instead of at two instants the host chose.
func FreezeWall(s *SimClock, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wallFn = func() time.Time { return at }
}
