package countgroup

// state represents the debounced occupancy of a single advanced zone.
type state int

const (
	stateUnknown state = iota
	stateOff
	stateOn
)

// debouncer applies N-of-M hysteresis to a stream of raw occupancy samples.
//
// Asymmetric by design: 2-of-3 on (fast response when a robot enters)
// and 3-of-3 off (slower release to prevent a momentary drop from
// clearing the warning light). False-clear dominates false-alarm risk,
// so we lean toward staying ON.
//
// On cold start, the debouncer holds state=Unknown and does NOT emit
// any transition until it has seen max(onThreshold, offThreshold)
// consecutive same-direction samples. The PLC keeps its last latched
// light state during this window; if core was down longer than the
// deadman window the PLC has already forced lights ON as fail-safe.
type debouncer struct {
	onThreshold  int
	offThreshold int

	current state
	// consec counts consecutive samples matching a pending transition.
	// It resets to 1 whenever the sample direction flips.
	consec int
	// pending records the direction of the current run.
	pending state
}

func newDebouncer(onThreshold, offThreshold int) *debouncer {
	return &debouncer{
		onThreshold:  onThreshold,
		offThreshold: offThreshold,
		current:      stateUnknown,
		pending:      stateUnknown,
	}
}

// feed ingests one sample (true = zone occupied, false = empty) and
// returns (newState, changed). changed is true only on the tick where
// the debounced state transitions; callers should suppress emits when
// changed is false.
func (d *debouncer) feed(occupied bool) (state, bool) {
	incoming := stateOff
	if occupied {
		incoming = stateOn
	}

	if incoming == d.pending {
		d.consec++
	} else {
		d.pending = incoming
		d.consec = 1
	}

	threshold := d.offThreshold
	if incoming == stateOn {
		threshold = d.onThreshold
	}

	if d.consec >= threshold && incoming != d.current {
		d.current = incoming
		return d.current, true
	}
	return d.current, false
}

// forceOn flips state to On regardless of sample history. Used by the
// RDS-down fail-safe path. Returns true if this actually changed state
// (so callers can suppress duplicate emits).
func (d *debouncer) forceOn() bool {
	if d.current == stateOn {
		return false
	}
	d.current = stateOn
	d.pending = stateOn
	d.consec = d.onThreshold
	return true
}

// isUnknown reports whether the debouncer has not yet committed to
// either On or Off — i.e. it's still in cold-start fill.
func (d *debouncer) isUnknown() bool {
	return d.current == stateUnknown
}
