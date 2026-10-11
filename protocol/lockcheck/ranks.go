package lockcheck

// The locks the protocol packages declare. Core and the Edge both use them, as
// leaves: each guards a few fields and calls nothing while held, so they rank
// below every lock either app declares (shingo-core/internal/lockorder,
// shingo-edge/internal/lockorder, both under 1000).
type (
	EventBus     struct{}
	ClockDefault struct{}
	ManualClock  struct{}
	SimClock     struct{}
	SimTimer     struct{}
	DebugLog     struct{}
)

func (EventBus) Rank() int     { return 1000 }
func (ClockDefault) Rank() int { return 1001 }
func (ManualClock) Rank() int  { return 1002 }
func (SimClock) Rank() int     { return 1003 }
func (SimTimer) Rank() int     { return 1004 }
func (DebugLog) Rank() int     { return 1005 }

func (EventBus) Name() string     { return "eventbus.Bus.mu" }
func (ClockDefault) Name() string { return "clock.defaultMu" }
func (ManualClock) Name() string  { return "clock.Manual.mu" }
func (SimClock) Name() string     { return "clock.SimClock.mu" }
func (SimTimer) Name() string     { return "clock.sim timer mu" }
func (DebugLog) Name() string     { return "debuglog.Logger.mu" }
