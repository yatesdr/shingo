//go:build sim

package main

import (
	"path"
	"runtime"
	"strings"
)

// Caller tags for the sim statement counter: which part of the Edge sent a
// statement, read off the goroutine's stack at the moment it was sent.
const (
	tagTick  = "tick"  // the PLC counter-delta path: one counter change and everything it drives
	tagOrder = "order" // order events: Core's order and carrier messages, operator actions, the order manager
	tagLoop  = "loop"  // periodic loops: heartbeat, reconcilers, lineside reporter, outbox drainer, feeds
	tagPoll  = "poll"  // PLC polling that is not a counter change
	tagSim   = "sim"   // sim-only machinery: the sim operator, the fake PLC, demo seeding
	tagOther = "other" // no pattern matched; printed with its path, never folded into another tag
)

// callerPattern is one row of the classification table. A row matches a frame
// by function name (fn) or by module-relative source file (file); exactly one
// of the two is set. A row with no tag is a pass-through: the frame it matches
// decides nothing and the walk continues outward.
//
// fn is either a whole function name, which also matches that function's
// closures ("....handleCounterDelta" matches "....handleCounterDelta.func1"
// but not "....handleCounterDeltaX"), or a package / receiver prefix ending
// in "." ("shingoedge/orders." is every function of the order manager).
//
// file is a prefix of the frame's file as "pkgdir/base.go" relative to its
// module ("engine/operator_" is every operator action file).
type callerPattern struct {
	tag  string
	fn   string
	file string
}

// callerTable classifies a statement's stack. The stack is walked from the
// innermost frame outward and the first frame that matches any row decides;
// within one frame the earlier row wins. Rows are entry points (subscribers,
// message handlers, loop bodies, operator actions), not shared helpers, so the
// innermost entry point a statement was sent under is the one it is charged
// to: an operator action the sim operator presses is "order" (it costs the
// same when a person presses it), while the sim operator's own reads are
// "sim"; an order the tick path places through the order manager is "order".
var callerTable = []callerPattern{
	// Pass-through (no tag): shared helpers that live in operator_ files but
	// are read by loops and by the sim operator as much as by operator
	// actions. A frame here matches no tag and the walk goes on outward, so
	// the sim operator's once-a-pass sweep through loadActiveNode is "sim" and
	// the demand sweep's CanAcceptOrders guard is "loop".
	{tag: "", file: "engine/operator_helpers.go"},
	{tag: "", file: "engine/operator_guards.go"},

	// sim: before www and plc, whose package prefixes would otherwise take
	// the sim routes' frames.
	{tag: tagSim, file: "engine/sim_operator.go"},
	{tag: tagSim, file: "engine/sim_machine_ready.go"},
	{tag: tagSim, file: "service/sim_demo_seed.go"},
	{tag: tagSim, file: "www/sim_routes.go"},
	{tag: tagSim, file: "cmd/shingoedge/sim_enabled.go"}, // the sim wiring: the fake PLC's machine-ready gate
	{tag: tagSim, fn: "shingoedge/plc/simwarlink."},

	// tick: the counter change, as the PLC manager writes it and as the
	// engine and its subscribers apply it, and the shipping of what it wrote.
	{tag: tagTick, fn: "shingoedge/plc.(*Manager).pollReportingPoint"},
	{tag: tagTick, fn: "shingoedge/engine.(*Engine).handleCounterDelta"},
	{tag: tagTick, file: "engine/wiring_counter_delta.go"},
	{tag: tagTick, fn: "shingoedge/engine.(*HourlyTracker).HandleDelta"},
	{tag: tagLoop, fn: "shingoedge/messaging.(*TickShipper).Lag"}, // read by the heartbeat
	{tag: tagTick, fn: "shingoedge/messaging.(*TickShipper)."},
	{tag: tagTick, fn: "shingoedge/uop.(*accumulator)."},

	// order
	{tag: tagOrder, fn: "shingoedge/messaging.(*EdgeHandler)."},
	{tag: tagOrder, fn: "shingoedge/orders."},
	{tag: tagOrder, fn: "shingoedge/engine.(*Engine).HandleOrderStatusSnapshots"},
	{tag: tagOrder, fn: "shingoedge/engine.(*Engine).HandleUnlistedOrders"},
	{tag: tagOrder, fn: "shingoedge/engine.(*Engine).drainIntents"},
	{tag: tagOrder, file: "engine/operator_"},
	{tag: tagOrder, file: "engine/wiring_completion.go"},
	{tag: tagOrder, file: "engine/wiring_delivered.go"},
	{tag: tagOrder, file: "engine/wiring_status_changed.go"},
	{tag: tagOrder, file: "engine/handler_bin_landed.go"},
	{tag: tagOrder, file: "engine/handler_bin_picked_up.go"},
	{tag: tagOrder, file: "engine/handler_bin_return.go"},
	{tag: tagOrder, file: "engine/handler_bin_epoch_refresh.go"},
	{tag: tagOrder, file: "engine/handler_uop_adjustment.go"},
	{tag: tagOrder, file: "engine/order_projection.go"},
	{tag: tagOrder, fn: "shingoedge/www."}, // HTTP: operator actions and the screens they read

	// loop
	{tag: tagLoop, fn: "shingoedge/messaging.(*Heartbeater)."},
	{tag: tagLoop, fn: "shingoedge/messaging.(*PlantClaimsPublisher)."},
	{tag: tagLoop, fn: "shingoedge/messaging.(*edgeOutboxStore)."},
	{tag: tagLoop, fn: "shingo/protocol/outbox.(*Drainer)."},
	{tag: tagLoop, fn: "shingoedge/engine.(*Engine).wakeIntentNodes"},
	{tag: tagLoop, fn: "shingoedge/engine.(*Engine).HandlePayloadCatalog"},
	{tag: tagLoop, file: "engine/demand_reconciler.go"},
	{tag: tagLoop, file: "engine/lineside_reporter.go"},
	{tag: tagLoop, file: "engine/uop_stranded_monitor.go"},
	{tag: tagLoop, file: "engine/reconciliation"},
	{tag: tagLoop, file: "engine/core_sync_service.go"},
	{tag: tagLoop, file: "engine/feeds"},
	{tag: tagLoop, file: "engine/node_feed.go"},
	{tag: tagLoop, file: "engine/supply_refusal_feed.go"},
	{tag: tagLoop, file: "engine/containment_feed.go"},
	{tag: tagLoop, file: "engine/handler_sourcing_state.go"},
	{tag: tagLoop, fn: "shingoedge/store.(*DB).CheckpointWAL"},
	{tag: tagLoop, fn: "shingoedge/store.(*DB).VacuumIfFragmented"},
	{tag: tagLoop, fn: "shingoedge/store/counters.PurgeOldSnapshots"},

	// poll: the PLC manager's own reads and the tag monitors. After the tick
	// row for pollReportingPoint, which this package prefix would also match.
	{tag: tagPoll, fn: "shingoedge/plc."},
	{tag: tagPoll, file: "engine/plc_catid_monitor.go"},
	{tag: tagPoll, file: "engine/plc_tag_poll.go"},
	{tag: tagPoll, file: "engine/plc_tag_derive.go"},
}

// callerFrame is one stack frame: the full function name and the source file
// relative to its module ("engine/wiring_counter_delta.go").
type callerFrame struct {
	fn   string
	file string
}

func (p callerPattern) matches(f callerFrame) bool {
	if p.file != "" {
		return strings.HasPrefix(f.file, p.file)
	}
	if !strings.HasPrefix(f.fn, p.fn) {
		return false
	}
	return strings.HasSuffix(p.fn, ".") || len(f.fn) == len(p.fn) || f.fn[len(p.fn)] == '.'
}

// classifyFrames returns the tag of a stack (innermost frame first) and its
// code path. A classified stack's path is the classifying frame, then the
// first frame outside the store and database/sql (the code that issued the
// read) when that is a different frame: "entry > reader". An unclassified
// stack's path is its outermost Edge frame (the goroutine's root) and the
// reader, so the row that should take it can be written from the output.
func classifyFrames(frames []callerFrame) (tag, codePath string) {
	reader := -1
	for i, f := range frames {
		if reader < 0 && !infraFrame(f.fn) {
			reader = i
		}
	}
	for i, f := range frames {
		for _, p := range callerTable {
			if !p.matches(f) {
				continue
			}
			if p.tag == "" {
				break // a shared helper: the frame decides nothing, walk on
			}
			return p.tag, joinPath(frames, i, reader)
		}
	}
	root := -1
	for i := len(frames) - 1; i >= 0; i-- {
		if !infraFrame(frames[i].fn) && !strings.HasPrefix(frames[i].fn, "runtime.") {
			root = i
			break
		}
	}
	return tagOther, joinPath(frames, root, reader)
}

func joinPath(frames []callerFrame, entry, reader int) string {
	switch {
	case entry < 0 && reader < 0:
		return "-"
	case entry < 0:
		return frameText(frames[reader])
	case reader < 0 || reader == entry:
		return frameText(frames[entry])
	}
	return frameText(frames[entry]) + " > " + frameText(frames[reader])
}

// infraFrame is a frame that sends a statement rather than decides to: the
// runtime, database/sql and its driver, the Edge store, and this counter.
func infraFrame(fn string) bool {
	pkg := funcPackage(fn)
	switch {
	case pkg == "runtime", pkg == "database/sql", pkg == "sync", pkg == "context",
		strings.HasPrefix(pkg, "modernc.org/"),
		pkg == "shingoedge/store", strings.HasPrefix(pkg, "shingoedge/store/"):
		return true
	}
	// This counter, under either name its package has: "main" in the binary,
	// its import path in a test binary.
	return strings.Contains(fn, ".(*stmtObserver).")
}

// funcPackage is the import path of a runtime function name:
// "shingoedge/engine.(*Engine).x.func1" -> "shingoedge/engine".
func funcPackage(fn string) string {
	slash := strings.LastIndexByte(fn, '/')
	if dot := strings.IndexByte(fn[slash+1:], '.'); dot >= 0 {
		return fn[:slash+1+dot]
	}
	return fn
}

// moduleDir maps a package import path to its directory in the repository, so
// a frame reads the same whatever directory the binary was built in.
func moduleDir(pkg string) string {
	switch {
	case pkg == "main":
		return "cmd/shingoedge"
	case strings.HasPrefix(pkg, "shingoedge/"):
		return strings.TrimPrefix(pkg, "shingoedge/")
	case strings.HasPrefix(pkg, "shingo/"):
		return strings.TrimPrefix(pkg, "shingo/")
	}
	return pkg
}

// frameText is "pkgdir/file.go:Func" with the package path dropped from the
// function name.
func frameText(f callerFrame) string {
	pkg := funcPackage(f.fn)
	return f.file + ":" + strings.TrimPrefix(f.fn[len(pkg):], ".")
}

// stackFrames resolves program counters to frames, innermost first.
func stackFrames(pcs []uintptr) []callerFrame {
	var out []callerFrame
	frames := runtime.CallersFrames(pcs)
	for {
		fr, more := frames.Next()
		if fr.Function != "" {
			out = append(out, callerFrame{
				fn:   fr.Function,
				file: moduleDir(funcPackage(fr.Function)) + "/" + path.Base(strings.ReplaceAll(fr.File, `\`, "/")),
			})
		}
		if !more {
			break
		}
	}
	return out
}
