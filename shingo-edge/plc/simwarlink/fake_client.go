//go:build sim

// Package simwarlink is a fake plc.WarlinkClient for sim mode (brief T3.1). It
// is anchored on the discovery handshake (ListPLCs + ListTags) so the REAL
// plc.Manager poll pipeline runs against it unchanged: poll loop → ListTags →
// cache → pollReportingPoint → CalculateDelta → enqueueProductionTick. There is
// no SSE implementation (S3) — dev config uses poll mode and OpenEventStream
// returns a reader that never delivers.
package simwarlink

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	_ "sync" // the plain lock inlines only where sync is imported (protocol/lockcheck)
	"time"

	"shingo/protocol/clock"
	"shingo/protocol/lockcheck"
	"shingoedge/config"
	"shingoedge/internal/lockorder"
	"shingoedge/plc"
)

var _ plc.WarlinkClient = (*FakeClient)(nil)

// Identity is a part-identity value, a process's CATID, for a sim run to set
// through WriteTagValue. A plant's PLC publishes it in the process's MES struct
// beside the counter; the fake holds it as a DINT that never ticks. It is a type
// of its own so an ordinary integer write (a zone light, a heartbeat) is still
// discarded.
type Identity int64

// Overrun is the overrun switch of one PLC, for a sim run to set through
// WriteTagValue on OverrunTag. With it on, the readiness gate lets the PLC keep
// counting while its process's produce seat is full or holds no carrier (the
// cell an operator runs parts to the table with); see engine.SimMachineReady.
// It is a type of its own so no ordinary bool write (a curtain) can set it.
type Overrun bool

// OverrunTag is the name the overrun switch is written and read under. It is
// not a published tag: ListTags never returns it, so the Edge's poll does not
// see it, and only a direct read (plc.Manager.ReadTagDirect) answers it.
const OverrunTag = "SimOverrun"

// ProgramChange is a program change to this identity, written through
// WriteTagValue on the counter tag of the process it changes: loading stops,
// the line drains, then the identity changes and the counter resets
// (Line.Change). ForcedChange is the same change without the drain
// (Line.Force). Each is a type of its own so no ordinary integer write can
// change a program.
type (
	ProgramChange int64
	ForcedChange  int64
)

// CountingSlot moves the count of the process counting on the tag it is
// written on (its configured counter) to slot N of its MES struct
// (Line.CountOn): Prod_Counter_0N ticks from then, the slot counted until
// then stops, and CATID_0N reads the part. A type of its own, like
// ProgramChange, so no ordinary integer write moves a count.
type CountingSlot int

// LineTag is the name a PLC's lines are read under: a direct read returns
// []ProcessLine for the processes counting on that PLC. Like OverrunTag it is
// never published to the poll.
const LineTag = "SimLine"

// ProcessLine is one process's line ledger as a read of LineTag returns it.
type ProcessLine struct {
	PLCName string `json:"plc_name"`
	TagName string `json:"tag_name"`
	LineState
}

// procLine is one sim process: its line and the tags the line publishes. mes
// is the MES struct the process counts on when its counter is a slot of one
// and its identity that slot's CATID ("" otherwise): the fake then publishes
// the struct's members as WarLink does (publishStruct).
type procLine struct {
	plcName, counterTag, stationTag, identityTag string
	mes                                          string
	line                                         *Line
}

// ReadinessFunc reports whether the fake PLC should emit a tick for the given
// plcName. When nil or returns true, the tick proceeds. When it returns false,
// the tick is suppressed (the machine is not ready — starved input, no output
// bin, or off shift). Set by the edge composition root to inspect runtime states.
type ReadinessFunc func(plcName string) bool

// FakeClient serves a fixed set of PLCs (one per distinct plc_name in the sim
// process list), each with counter tags that climb on a clock ticker — standing
// in for presses/lines whose PLCs WarLink would normally poll.
type FakeClient struct {
	mu   lockcheck.RWMutex[lockorder.SimPLC]
	plcs []string                    // distinct PLC names, sorted (stable output)
	vals map[string]map[string]int64 // plcName → tagName → counter (production and station)
	// bools are writable BOOL tags: a light curtain's state, standing in for
	// the cell's bypass button (S7). A bool write creates or sets one; the
	// counters stay unwritable.
	bools map[string]map[string]bool
	// idents are part-identity tags (a process's CATID), set through
	// WriteTagValue with an Identity and published as a DINT that never ticks.
	idents map[string]map[string]int64
	// overrun holds the PLCs whose overrun switch is on (Overrun).
	overrun map[string]bool
	// lines are the sim processes by PLC, in config order. Each tick advances
	// a line and publishes its counter, station clicks and identity.
	lines map[string][]*procLine
	// enabled are the MES struct members the Edge asked WarLink to publish
	// (SetTagPublishing), by PLC: a sibling slot's counter or CATID is
	// published once enabled, as a member WarLink is not configured with is.
	enabled map[string]map[string]bool
	clk     clock.Clock
	ready   ReadinessFunc // nil = always ready (backward-compatible)
}

// NewFakeClient builds the fake from the sim process list and starts one ticker
// goroutine per process (under ctx) that increments its counter by uop_per_tick
// every tick_interval. ctx owns the tickers' lifetime; edge main passes a
// process-lived context (the sim runs until exit, like the core driver).
// SetReadinessFunc sets the per-process readiness gate (G3). Call before the
// first tick or at any time — the check is read under RLock on each tick.
// Nil means always ready (the default, backward-compatible behavior).
func (f *FakeClient) SetReadinessFunc(fn ReadinessFunc) {
	f.mu.Lock()
	f.ready = fn
	f.mu.Unlock()
}

func NewFakeClient(ctx context.Context, cfg config.SimConfig, clk clock.Clock) *FakeClient {
	f := &FakeClient{vals: make(map[string]map[string]int64), bools: make(map[string]map[string]bool), idents: make(map[string]map[string]int64), overrun: make(map[string]bool), lines: make(map[string][]*procLine), enabled: make(map[string]map[string]bool), clk: clk}
	procs := make([]*procLine, 0, len(cfg.Processes))
	for _, p := range cfg.Processes {
		if f.vals[p.PLCName] == nil {
			f.vals[p.PLCName] = make(map[string]int64)
			f.plcs = append(f.plcs, p.PLCName)
		}
		per := p.UOPPerTick
		if per <= 0 {
			per = 1
		}
		pl := &procLine{plcName: p.PLCName, counterTag: p.TagName, stationTag: p.StationTag, identityTag: p.IdentityTag, line: NewLine(p.LineDepth, per)}
		if s, n, ok := MESSlotOf(p.TagName); ok && p.IdentityTag == s+"."+CATIDMember(n) {
			pl.mes = s
			_ = pl.line.CountOn(n) // n is a slot: MESSlotOf says so
		}
		if pl.identityTag != "" && f.idents[p.PLCName] == nil {
			f.idents[p.PLCName] = make(map[string]int64)
		}
		f.lines[p.PLCName] = append(f.lines[p.PLCName], pl)
		f.publish(pl)
		procs = append(procs, pl)
	}
	sort.Strings(f.plcs)
	for i, p := range cfg.Processes {
		interval := p.TickInterval
		if interval <= 0 {
			interval = 3 * time.Second
		}
		// The clock applies the speed multiplier: a SimClock re-paces its tickers
		// live (NewTicker re-reads speed each cycle). Pre-scaling the interval here
		// too would double-count and break the live speed toggle.
		// Register the ticker synchronously (before returning) so a test using a
		// manual clock can Advance immediately without racing goroutine startup.
		go f.tick(ctx, procs[i], f.clk.NewTicker(interval))
	}
	return f
}

func (f *FakeClient) tick(ctx context.Context, pl *procLine, t clock.Ticker) {
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C():
			// Readiness gate (G3): a real PLC stops counting when the machine
			// isn't ready (starved input, no output bin, off shift). The fake
			// checks the injected ReadinessFunc and stalls the line (nothing
			// loads or counts) when the machine should idle.
			f.mu.RLock()
			ready := f.ready
			f.mu.RUnlock()
			cycles := ready == nil || ready(pl.plcName)
			f.mu.Lock()
			if cycles {
				pl.line.Tick()
			} else {
				pl.line.Stall()
			}
			f.publish(pl)
			f.mu.Unlock()
		}
	}
}

// publish copies a line's counter, station clicks and identity into the tags
// the poll reads. Caller holds f.mu.
func (f *FakeClient) publish(pl *procLine) {
	if pl.stationTag != "" {
		f.vals[pl.plcName][pl.stationTag] = pl.line.station
	}
	if pl.mes != "" {
		f.publishStruct(pl)
		return
	}
	f.vals[pl.plcName][pl.counterTag] = pl.line.counter
	if pl.identityTag != "" {
		f.idents[pl.plcName][pl.identityTag] = pl.line.identity
	}
}

// publishStruct publishes the members of a line's MES struct that WarLink
// publishes: the counter and identity tags it is configured with, and each
// member the Edge enabled. The other members answer a direct read only
// (structRead). Caller holds f.mu.
func (f *FakeClient) publishStruct(pl *procLine) {
	for i, s := range pl.line.Slots() {
		if tag := pl.mes + "." + CounterMember(i+1); tag == pl.counterTag || f.enabled[pl.plcName][tag] {
			f.vals[pl.plcName][tag] = s.Counter
		}
		if tag := pl.mes + "." + CATIDMember(i+1); tag == pl.identityTag || f.enabled[pl.plcName][tag] {
			f.idents[pl.plcName][tag] = s.CATID
		}
	}
}

// structRead answers a direct read of a line's MES struct or of one of its
// slot members, published or not: the struct reads as its members by name, as
// WarLink reads a UDT. ok is false when tagName is neither. Caller holds f.mu.
func (f *FakeClient) structRead(plcName, tagName string) (any, bool) {
	for _, pl := range f.lines[plcName] {
		if pl.mes == "" || (tagName != pl.mes && !strings.HasPrefix(tagName, pl.mes+".")) {
			continue
		}
		members := make(map[string]any, 2*MESSlots)
		for i, s := range pl.line.Slots() {
			members[CATIDMember(i+1)] = s.CATID
			members[CounterMember(i+1)] = s.Counter
		}
		if tagName == pl.mes {
			return members, true
		}
		v, ok := members[strings.TrimPrefix(tagName, pl.mes+".")]
		return v, ok
	}
	return nil, false
}

// structOf is the line whose MES struct has tagName as a slot member, nil
// when none does. Caller holds f.mu.
func (f *FakeClient) structOf(plcName, tagName string) *procLine {
	for _, pl := range f.lines[plcName] {
		if pl.mes == "" {
			continue
		}
		for n := 1; n <= MESSlots; n++ {
			if tagName == pl.mes+"."+CounterMember(n) || tagName == pl.mes+"."+CATIDMember(n) {
				return pl
			}
		}
	}
	return nil
}

// ListPLCs returns every configured PLC as Connected — the literal status string
// plc.Manager.IsConnected compares against.
func (f *FakeClient) ListPLCs(ctx context.Context) ([]plc.WarlinkPLC, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]plc.WarlinkPLC, 0, len(f.plcs))
	for _, name := range f.plcs {
		out = append(out, plc.WarlinkPLC{
			Name:        name,
			Address:     "sim",
			Status:      "Connected",
			ProductName: "SimPLC",
		})
	}
	return out, nil
}

// ListTags returns the PLC's current counter values. Keys carry the "plcName."
// prefix the real WarLink uses (Manager.applyTags strips it); values are int64,
// which Manager.toInt64 accepts. Error stays empty so connectionErrorFromTags
// doesn't read it as a disconnect.
func (f *FakeClient) ListTags(ctx context.Context, plcName string) (map[string]plc.WarlinkTag, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	tags := f.vals[plcName]
	out := make(map[string]plc.WarlinkTag, len(tags)+len(f.bools[plcName])+len(f.idents[plcName]))
	for tagName, v := range tags {
		out[plcName+"."+tagName] = plc.WarlinkTag{
			PLC:   plcName,
			Name:  tagName,
			Type:  "DINT",
			Value: v,
		}
	}
	for tagName, v := range f.bools[plcName] {
		out[plcName+"."+tagName] = plc.WarlinkTag{PLC: plcName, Name: tagName, Type: "BOOL", Value: v}
	}
	for tagName, v := range f.idents[plcName] {
		out[plcName+"."+tagName] = plc.WarlinkTag{PLC: plcName, Name: tagName, Type: "DINT", Value: v}
	}
	return out, nil
}

// ListAllTags returns the same counters in the all-tags shape (HMI convenience).
func (f *FakeClient) ListAllTags(ctx context.Context, plcName string) ([]plc.WarlinkTagInfo, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	tags := f.vals[plcName]
	names := make([]string, 0, len(tags))
	for name := range tags {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]plc.WarlinkTagInfo, 0, len(names))
	for _, name := range names {
		out = append(out, plc.WarlinkTagInfo{
			Name:       name,
			Type:       "DINT",
			Configured: true,
			Enabled:    true,
			Value:      tags[name],
		})
	}
	return out, nil
}

// SetTagPublishing publishes a member of a process's MES struct the Edge
// enables (a sibling slot's counter or CATID: a re-point enables both) and
// stops publishing one it disables, but for the counter and identity tags the
// process is configured with, which stay published. Every other sim tag is
// already published: a no-op.
func (f *FakeClient) SetTagPublishing(ctx context.Context, plcName, tagName string, enabled bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	pl := f.structOf(plcName, tagName)
	if pl == nil {
		return nil
	}
	if enabled {
		if f.enabled[plcName] == nil {
			f.enabled[plcName] = make(map[string]bool)
		}
		f.enabled[plcName][tagName] = true
	} else {
		delete(f.enabled[plcName], tagName)
		if tagName != pl.counterTag && tagName != pl.identityTag {
			delete(f.vals[plcName], tagName)
			delete(f.idents[plcName], tagName)
		}
	}
	f.publish(pl)
	return nil
}

// ReadTagValue returns the current counter for a single tag (HMI convenience).
func (f *FakeClient) ReadTagValue(ctx context.Context, plcName, tagName string) (any, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if tagName == OverrunTag {
		if f.vals[plcName] == nil {
			return nil, fmt.Errorf("simwarlink: PLC %q has no counter to overrun", plcName)
		}
		return f.overrun[plcName], nil
	}
	if tagName == LineTag {
		if f.lines[plcName] == nil {
			return nil, fmt.Errorf("simwarlink: PLC %q has no line", plcName)
		}
		out := make([]ProcessLine, 0, len(f.lines[plcName]))
		for _, pl := range f.lines[plcName] {
			out = append(out, ProcessLine{PLCName: plcName, TagName: pl.counterTag, LineState: pl.line.State()})
		}
		return out, nil
	}
	if v, ok := f.structRead(plcName, tagName); ok {
		return v, nil
	}
	if b, ok := f.bools[plcName][tagName]; ok {
		return b, nil
	}
	if v, ok := f.idents[plcName][tagName]; ok {
		return v, nil
	}
	tags, ok := f.vals[plcName]
	if !ok {
		return nil, fmt.Errorf("simwarlink: PLC %q not found", plcName)
	}
	v, ok := tags[tagName]
	if !ok {
		return nil, fmt.Errorf("simwarlink: tag %q not found on %q", tagName, plcName)
	}
	return v, nil
}

// WriteTagValue sets a part-identity tag when the value is an Identity, changes
// the program of the process counting on tagName when it is a ProgramChange or
// a ForcedChange, moves its count to another slot of its MES struct when it is
// a CountingSlot, sets the PLC's overrun switch when it is an Overrun on
// OverrunTag, and a BOOL tag when it is a bool (a curtain's state, the sim's
// bypass button), registering its PLC if new; any other write is discarded (Q4:
// zone lights / heartbeats have no sim effect) and reports success. A counter
// is never written.
func (f *FakeClient) WriteTagValue(ctx context.Context, plcName, tagName string, value any) error {
	if id, ok := value.(Identity); ok {
		return f.setIdentity(plcName, tagName, int64(id))
	}
	if to, ok := value.(ProgramChange); ok {
		return f.changeProgram(plcName, tagName, func(l *Line) { l.Change(int64(to)) })
	}
	if to, ok := value.(ForcedChange); ok {
		return f.changeProgram(plcName, tagName, func(l *Line) { l.Force(int64(to)) })
	}
	if n, ok := value.(CountingSlot); ok {
		return f.moveCount(plcName, tagName, int(n))
	}
	if o, ok := value.(Overrun); ok || tagName == OverrunTag {
		if !ok || tagName != OverrunTag {
			return fmt.Errorf("simwarlink: the overrun switch is an Overrun written on %s", OverrunTag)
		}
		return f.setOverrun(plcName, bool(o))
	}
	b, ok := value.(bool)
	if !ok {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, counter := f.vals[plcName][tagName]; counter {
		return nil
	}
	if f.bools[plcName] == nil {
		f.bools[plcName] = make(map[string]bool)
		if f.vals[plcName] == nil && f.idents[plcName] == nil {
			f.plcs = append(f.plcs, plcName)
			sort.Strings(f.plcs)
		}
	}
	f.bools[plcName][tagName] = b
	return nil
}

// OpenEventStream returns a reader that stays open and never delivers. Poll mode
// never calls this; a closed/blocking reader would break a misconfigured sse
// mode, so we hand back a live pipe whose writer is never used (S3).
func (f *FakeClient) OpenEventStream(ctx context.Context) (io.ReadCloser, error) {
	r, _ := io.Pipe()
	return r, nil
}

// setIdentity sets a part-identity tag. A counter is never written: its name
// is refused, so a run cannot stop a counter by naming it.
func (f *FakeClient) setIdentity(plcName, tagName string, v int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, counter := f.vals[plcName][tagName]; counter {
		return fmt.Errorf("simwarlink: %s.%s is a counter, not an identity tag", plcName, tagName)
	}
	if f.idents[plcName] == nil {
		f.idents[plcName] = make(map[string]int64)
		if f.vals[plcName] == nil && f.bools[plcName] == nil {
			f.plcs = append(f.plcs, plcName)
			sort.Strings(f.plcs)
		}
	}
	for _, pl := range f.lines[plcName] {
		if pl.countingIdentity() == tagName {
			pl.line.SetIdentity(v)
		}
	}
	f.idents[plcName][tagName] = v
	return nil
}

// countingIdentity is the identity tag of the slot the process counts on now:
// its configured identity tag until its count moves to another slot of its MES
// struct, that slot's CATID after.
func (pl *procLine) countingIdentity() string {
	if pl.mes == "" {
		return pl.identityTag
	}
	return pl.mes + "." + CATIDMember(pl.line.CountingSlot())
}

// moveCount moves the count of the process counting on plcName.tagName (its
// configured counter) to slot n of its MES struct and publishes what moved. A
// process whose counter is not a slot of an MES struct is refused.
func (f *FakeClient) moveCount(plcName, tagName string, n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, pl := range f.lines[plcName] {
		if pl.counterTag != tagName {
			continue
		}
		if pl.mes == "" {
			return fmt.Errorf("simwarlink: %s.%s is not a slot of an MES struct (<struct>.Prod_Counter_0N, identity_tag its CATID_0N)", plcName, tagName)
		}
		if err := pl.line.CountOn(n); err != nil {
			return err
		}
		f.publish(pl)
		return nil
	}
	return fmt.Errorf("simwarlink: no sim process counts on %s.%s", plcName, tagName)
}

// changeProgram applies a program change to the process counting on
// plcName.tagName and publishes what it moved. The change is to the process's
// identity tag, so a process without one is refused: the Edge would see a
// counter reset and no new part.
func (f *FakeClient) changeProgram(plcName, tagName string, change func(*Line)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, pl := range f.lines[plcName] {
		if pl.counterTag != tagName {
			continue
		}
		if pl.identityTag == "" {
			return fmt.Errorf("simwarlink: %s.%s has no identity_tag for a program change to write", plcName, tagName)
		}
		change(pl.line)
		f.publish(pl)
		return nil
	}
	return fmt.Errorf("simwarlink: no sim process counts on %s.%s", plcName, tagName)
}

// setOverrun sets or clears a PLC's overrun switch. Only a PLC with a counter
// can be switched: on any other the switch would gate nothing, so a misspelt
// name is refused rather than kept.
func (f *FakeClient) setOverrun(plcName string, on bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.vals[plcName] == nil {
		return fmt.Errorf("simwarlink: PLC %q has no counter to overrun", plcName)
	}
	if on {
		f.overrun[plcName] = true
	} else {
		delete(f.overrun, plcName)
	}
	return nil
}

// Overrun reports whether a PLC's overrun switch is on. The readiness gate
// reads it on every tick of that PLC (one map read under the read lock).
func (f *FakeClient) Overrun(plcName string) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.overrun[plcName]
}
