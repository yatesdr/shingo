// catid_slot_watch.go — the MES sibling slots, watched.
//
// A process counts on one slot of its cell's MES struct (Prod_Counter_0N; its
// part identity CATID_0N, derived). The PLC may count the part on another
// slot: a cell whose program moves its count between slots leaves the counted
// one frozen while a sibling rises. The watch reads the struct's five
// (CATID_0N, Prod_Counter_0N) pairs once a minute on its own goroutine — from
// the manager's cache where WarLink publishes them, else in one read of the
// struct — and keeps what the last two readings say in memory. The strip reads
// it (slotItems). It decides nothing: the count, the identity, the cutover and
// the switch read the configured slot only, and a re-point is a person's click
// through the counter edit.

package engine

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"shingo/protocol/clock"
	"shingoedge/store/processes"
)

// The strip's slot item kinds.
const (
	catidKindSlotIdle     = "counter_slot_idle"
	catidKindSlotLockstep = "counter_slot_lockstep"
)

const (
	// slotWatchInterval is the watch's cadence: a slot that moved the count
	// is a standing condition, read in minutes, never per stroke.
	slotWatchInterval = time.Minute
	// mesSlots is the MES struct's slot count (Prod_Counter_01 … _05).
	mesSlots = 5
	// slotStructReadTimeout bounds the one struct read per process per pass.
	slotStructReadTimeout = 5 * time.Second
)

// slotState is one slot of the struct as the watch last read it.
type slotState struct {
	tag   string // <struct>.Prod_Counter_0N
	catid string
	count int64
	read  bool // both members read at the last pass
	// ledSince is when this slot first rose at a pass where the counted slot
	// did not, since the counted slot last rose; zero when it has not.
	ledSince time.Time
	// lockstep: the last time this slot rose, the counted slot rose too.
	lockstep bool
	// rising: it rose at the last pass; roseAt: the last pass it rose at,
	// zero when the watch has not seen it rise (the setup page shows both).
	rising bool
	roseAt time.Time
}

// slotFacts is one process's slots as the watch published them, for the
// counter tag they were read against: after a re-point they are stale until
// the next pass (the strip ignores them).
type slotFacts struct {
	counterTag string
	counted    int // the counted slot's index, 0-based
	slots      [mesSlots]slotState
}

// MESSlot is one slot of a process's MES struct as the watch last read it,
// for the setup page: its CATID (Part: it names one), its count, whether it
// rose at the last pass and when it last did, whether the process counts on
// it, and the reading's label (the watch's reading: discovered).
type MESSlot struct {
	Slot    int        `json:"slot"`
	Tag     string     `json:"tag"`
	CATID   string     `json:"catid"`
	Part    bool       `json:"part"`
	Count   int64      `json:"count"`
	Read    bool       `json:"read"`
	Rising  bool       `json:"rising"`
	RoseAt  *time.Time `json:"rose_at,omitempty"`
	Counted bool       `json:"counted"`
	Label   string     `json:"label"`
}

// slotSuffix is a slot's two-digit member index, 1-based.
func slotSuffix(n int) string { return fmt.Sprintf("%02d", n) }

// mesSlotOf names a process's MES struct and its counted slot (0-based) when
// the process counts on Prod_Counter_0N of a struct, N in 1..5, and reads its
// identity by derivation from that slot. A typed identity tag does not follow
// a re-point, so a process with one is not watched.
func mesSlotOf(p *processes.Process) (prefix, base string, counted int, ok bool) {
	tag := identityTagOf(p)
	if tag == "" || tag != deriveIdentityTag(p.CounterTagName) {
		return "", "", 0, false
	}
	prefix = deriveProcessTagPrefix(p.CounterTagName)
	leaf := p.CounterTagName[len(prefix)+1:]
	if len(leaf) != len("Prod_Counter_01") || !strings.EqualFold(leaf[:len(leaf)-2], "Prod_Counter_") {
		return "", "", 0, false
	}
	for n := 1; n <= mesSlots; n++ {
		if leaf[len(leaf)-2:] == slotSuffix(n) {
			return prefix, leaf[:len(leaf)-2], n - 1, true
		}
	}
	return "", "", 0, false
}

// runSlotWatch is the watch's goroutine: a pass a minute until the engine
// stops. It never runs on the monitor's tick.
func (cm *catidMonitor) runSlotWatch() {
	ticker := time.NewTicker(slotWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-cm.eng.stopChan:
			return
		case <-ticker.C:
			cm.watchSlots(clock.Now())
		}
	}
}

// watchSlots is one pass: one statement (the process list), then each watched
// process's slots from the cache or one struct read, and the whole map
// published in one store. It holds none of the monitor's locks: a struct read
// that hangs delays the next pass, never a tick or the guard
// (TestSlotWatch_NeverTakesTheTickPath).
func (cm *catidMonitor) watchSlots(now time.Time) {
	procs, err := cm.eng.db.ListProcesses()
	if err != nil {
		log.Printf("plc-catid: slot watch: list processes: %v", err)
		return
	}
	var prev map[int64]*slotFacts
	if m := cm.slots.Load(); m != nil {
		prev = *m
	}
	next := make(map[int64]*slotFacts, len(procs))
	for i := range procs {
		p := &procs[i]
		prefix, base, counted, ok := mesSlotOf(p)
		if !ok {
			continue
		}
		read := cm.readSlots(p.CounterPLCName, prefix, base)
		next[p.ID] = advanceSlots(prev[p.ID], p.CounterTagName, counted, read, now)
	}
	cm.slots.Store(&next)
}

// readSlots reads the struct's five pairs: each member from the manager's
// cache, and, when any is not there and the PLC is connected, the struct
// once, directly. Nothing is published for it: each member WarLink publishes
// is a config entry it keeps.
func (cm *catidMonitor) readSlots(plcName, prefix, base string) [mesSlots]slotState {
	var out [mesSlots]slotState
	missing := false
	for i := range out {
		n := slotSuffix(i + 1)
		out[i].tag = prefix + "." + base + n
		c, errC := cm.eng.plcMgr.ReadTag(plcName, prefix+".CATID_"+n)
		v, errV := cm.eng.plcMgr.ReadTag(plcName, out[i].tag)
		if errC != nil || errV != nil || !out[i].take(c, v) {
			missing = true
		}
	}
	if !missing || !cm.eng.plcMgr.IsConnected(plcName) {
		return out
	}
	ctx, cancel := context.WithTimeout(context.Background(), slotStructReadTimeout)
	raw, err := cm.eng.plcMgr.ReadTagDirect(ctx, plcName, prefix)
	cancel()
	members, ok := raw.(map[string]any)
	if err != nil || !ok {
		return out
	}
	for i := range out {
		if !out[i].read {
			n := slotSuffix(i + 1)
			out[i].take(memberOf(members, "CATID_"+n), memberOf(members, base+n))
		}
	}
	return out
}

// take sets the slot's reading from a CATID and a counter value; false when
// either is not a value of its kind.
func (s *slotState) take(catid, count any) bool {
	c, okC := catidToString(catid)
	v, okV := plcTagInt64(count)
	if !okC || !okV {
		return false
	}
	s.catid, s.count, s.read = c, v, true
	return true
}

// memberOf is a struct read's member by name, the case as the PLC spells it
// or not.
func memberOf(members map[string]any, name string) any {
	if v, ok := members[name]; ok {
		return v
	}
	for k, v := range members {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return nil
}

// advanceSlots carries a process's slots one pass forward (pure): a slot that
// rose at a pass where the counted slot was read twice and did not rise leads
// from then until the counted slot rises; a slot that rose with it is in
// lockstep; each slot that rose is rising, and remembers the pass. A reading
// against another counter tag (a re-point since) starts over.
func advanceSlots(prev *slotFacts, counterTag string, counted int, read [mesSlots]slotState, now time.Time) *slotFacts {
	f := &slotFacts{counterTag: counterTag, counted: counted, slots: read}
	if prev == nil || prev.counterTag != counterTag {
		return f
	}
	countedRead := prev.slots[counted].read && read[counted].read
	countedRose := rose(prev.slots[counted], read[counted])
	for i := range f.slots {
		s, p := &f.slots[i], prev.slots[i]
		s.ledSince, s.lockstep, s.roseAt = p.ledSince, p.lockstep, p.roseAt
		if s.rising = rose(p, *s); s.rising {
			s.roseAt = now
		}
		if i != counted && countedRead && s.rising {
			s.lockstep = countedRose
			if !countedRose && s.ledSince.IsZero() {
				s.ledSince = now
			}
		}
		if countedRose {
			s.ledSince = time.Time{}
		}
	}
	return f
}

// rose reports whether a slot's counter went up between two readings. A
// reset (the counter going down) is not a rise.
func rose(before, after slotState) bool {
	return before.read && after.read && after.count > before.count
}

// slotFactsOf is a process's published slots, nil when the watch has none.
// No lock.
func (cm *catidMonitor) slotFactsOf(processID int64) *slotFacts {
	m := cm.slots.Load()
	if m == nil {
		return nil
	}
	return (*m)[processID]
}

// mesSlots is a process's slots as the watch last published them, for the
// setup page ([] when it has none, or read them against another counter tag):
// one load of the published pointer, no statement, no lock.
func (e *Engine) mesSlots(proc *processes.Process) []MESSlot {
	out := []MESSlot{}
	if e.catidMon == nil {
		return out
	}
	f := e.catidMon.slotFactsOf(proc.ID)
	if f == nil || f.counterTag != proc.CounterTagName {
		return out
	}
	for i, s := range f.slots {
		m := MESSlot{Slot: i + 1, Tag: s.tag, CATID: s.catid, Part: s.read && isIdentity(s.catid), Count: s.count,
			Read: s.read, Rising: s.rising, Counted: i == f.counted, Label: "discovered"}
		if !s.roseAt.IsZero() {
			t := s.roseAt
			m.RoseAt = &t
		}
		out = append(out, m)
	}
	return out
}

// slotItems: the PLC counts on a sibling slot whose CATID names a part while
// the counted slot is flat (red, with the click that re-points the counter),
// or a sibling naming a part rises with the counted slot (info: a second
// counter of the same stroke, counted once). A slot reading -1 or 0 names no
// part and raises nothing.
func slotItems(proc *processes.Process, f *slotFacts) []CatidHealthItem {
	if f == nil || f.counterTag != proc.CounterTagName {
		return nil
	}
	counted := leafOf(f.slots[f.counted].tag)
	var items []CatidHealthItem
	for i, s := range f.slots {
		if i == f.counted || !s.read || !isIdentity(s.catid) {
			continue
		}
		slot := leafOf(s.tag)
		data := map[string]any{"tag": s.tag, "slot": i + 1, "catid": s.catid, "counted_tag": proc.CounterTagName}
		switch {
		case !s.ledSince.IsZero():
			t := s.ledSince
			items = append(items, CatidHealthItem{Kind: catidKindSlotIdle, Severity: CatidHealthRed,
				Headline: fmt.Sprintf("The PLC counts on %s (CATID %s); this process counts %s, which has not moved", slot, s.catid, counted),
				Detail:   fmt.Sprintf("count %s: the part identity follows to CATID_%s", slot, slotSuffix(i+1)),
				Since:    &t, Data: data})
		case s.lockstep:
			items = append(items, CatidHealthItem{Kind: catidKindSlotLockstep, Severity: CatidHealthInfo,
				Headline: fmt.Sprintf("%s (CATID %s) rises with %s: a second counter of the same stroke", slot, s.catid, counted),
				Detail:   "counted once, on " + counted, Data: data})
		}
	}
	return items
}

// leafOf is a tag's member name, after its struct.
func leafOf(tag string) string {
	if i := strings.LastIndex(tag, "."); i >= 0 {
		return tag[i+1:]
	}
	return tag
}
