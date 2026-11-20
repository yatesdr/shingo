package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	"shingo/protocol"
	"shingocore/fleet"
	"shingocore/fleet/seerrds"
	"shingocore/store/nodes"
)

// table_turn.go — CORE TURNS A ROBOT'S JACK TABLE AT A CONFIGURED NODE.
//
// At a node with a table angle, Core turns the robot's jack table to that
// angle while the robot is parked at a wait there, and only then sends the lift
// (or, on a set-down node, the set-down). The turn is one #3057 through the
// fleet's relay (fleet.TableTurner); Core knows it is done from the table angle
// on the robot status it already polls (fleet.TableOnAngle).
//
// Design: turntable-bintask-2026-11-10/PLAN-turn-before-lift-2026-11-10.md
// (§1 to §4), after three review rounds (SYNTH-round3.md §3 to §6). The robot
// evidence is BRIEF-turn-before-lift-2026-11-10.md §2: a binTask cannot turn
// the table, the relay call can, and the table holds its angle through the
// lift that follows.
//
// This file is the configuration: the two node properties, the door that
// refuses a configuration that cannot work, and the in-memory set of turn nodes
// every reader asks, so that a node with no angle costs no query.

// The node properties.
const (
	// PropTableTurnRad is the table angle at a node, radians in the robot's
	// frame, 0 < |a| < π. Unset (or empty) is off: there is no separate enable.
	// The node page shows and takes degrees and stores radians.
	PropTableTurnRad = "table_turn_rad"
	// PropTableTurnSide is when the turn happens: TableTurnSideLift (before the
	// lift, the default when unset) or TableTurnSideSetDown (before the
	// set-down). One side per node, which a single key makes structural.
	PropTableTurnSide = "table_turn_side"
)

// The two sides.
const (
	TableTurnSideLift    = "lift"
	TableTurnSideSetDown = "set_down"
)

// turnNode is one node's turn: the angle and the side.
type turnNode struct {
	rad  float64
	side string
}

// RobotStatusReader is one robot's latest polled status, and whether it is
// both present and fresh. Wired by the engine from the poll it already makes.
type RobotStatusReader func(vehicleID string) (fleet.RobotStatus, bool)

// SetTableTurner wires the fleet's table turner and the robot status reader,
// and loads the turn nodes. A nil turner is a fleet that cannot turn a table:
// the door then refuses every angle.
func (d *Dispatcher) SetTableTurner(t fleet.TableTurner, status RobotStatusReader) {
	d.turnMu.Lock()
	d.turner, d.robotStatus = t, status
	d.turnMu.Unlock()
	if err := d.ReloadTableTurns(); err != nil {
		log.Printf("dispatch: table turn: could not load the turn nodes: %v (the next property write or the floor reloads them)", err)
	}
}

// ReloadTableTurns re-reads every node's turn configuration into the cache.
// Called at start and by the property doors after a write; the floor calls it
// too, and logs when what it read disagrees with what it held (a write that
// went around the doors).
func (d *Dispatcher) ReloadTableTurns() error {
	props, err := d.db.ListNodePropertiesByKeys(PropTableTurnRad, PropTableTurnSide)
	if err != nil {
		return err
	}
	byNode := map[string]turnNode{}
	for _, p := range props {
		t := byNode[p.NodeName]
		switch p.Key {
		case PropTableTurnRad:
			rad, ok := parseTurnRad(p.Value)
			if !ok {
				if strings.TrimSpace(p.Value) != "" {
					log.Printf("dispatch: table turn: node %s has an unreadable angle %q — no turn there", p.NodeName, p.Value)
				}
				continue
			}
			t.rad = rad
		case PropTableTurnSide:
			t.side = p.Value
		}
		byNode[p.NodeName] = t
	}
	next := map[string]turnNode{}
	for name, t := range byNode {
		if t.rad == 0 {
			continue // a side with no angle is off
		}
		if t.side == "" {
			t.side = TableTurnSideLift
		}
		next[name] = t
	}
	d.turnMu.Lock()
	prev := d.turnNodes
	d.turnNodes = next
	d.turnMu.Unlock()
	if prev != nil && !sameTurnNodes(prev, next) {
		log.Printf("dispatch: table turn: the turn nodes changed on reload (%d → %d) — %s",
			len(prev), len(next), describeTurnNodes(next))
	}
	return nil
}

// sameTurnNodes reports whether two cached sets agree.
func sameTurnNodes(a, b map[string]turnNode) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// describeTurnNodes names a set for a log line.
func describeTurnNodes(m map[string]turnNode) string {
	if len(m) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(m))
	for name, t := range m {
		parts = append(parts, fmt.Sprintf("%s %.1f° before the %s", name, t.rad*180/math.Pi, sideWords(t.side)))
	}
	return strings.Join(parts, ", ")
}

// sideWords is a side in a sentence.
func sideWords(side string) string {
	if side == TableTurnSideSetDown {
		return "set-down"
	}
	return "lift"
}

// turnAt is the turn configured at a node, from the cache: no query.
func (d *Dispatcher) turnAt(node string) (turnNode, bool) {
	if node == "" {
		return turnNode{}, false
	}
	d.turnMu.Lock()
	defer d.turnMu.Unlock()
	t, ok := d.turnNodes[node]
	return t, ok
}

// hasTurnNodes reports whether any node carries a turn: the one check a node
// with no angle pays.
func (d *Dispatcher) hasTurnNodes() bool {
	d.turnMu.Lock()
	defer d.turnMu.Unlock()
	return len(d.turnNodes) > 0
}

// tableTurner is the wired turner, nil when the fleet has none.
func (d *Dispatcher) tableTurner() (fleet.TableTurner, RobotStatusReader) {
	d.turnMu.Lock()
	defer d.turnMu.Unlock()
	return d.turner, d.robotStatus
}

// parseTurnRad reads a stored angle. ok is false for unset or unreadable.
func parseTurnRad(v string) (float64, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	rad, err := strconv.ParseFloat(v, 64)
	if err != nil || !validTurnRad(rad) {
		return 0, false
	}
	return rad, true
}

// validTurnRad is the door's angle rule: finite and 0 < |a| < π. Zero is the
// table's rest and needs no turn; ±π is ambiguous between the two ways round.
func validTurnRad(rad float64) bool {
	return !math.IsNaN(rad) && !math.IsInf(rad, 0) && rad != 0 && math.Abs(rad) < math.Pi
}

// ErrTableTurnRefused marks a property write the door refused.
var ErrTableTurnRefused = errors.New("table turn refused")

// IsTableTurnProperty reports whether key is one of the turn's properties.
func IsTableTurnProperty(key string) bool {
	return key == PropTableTurnRad || key == PropTableTurnSide
}

// CheckTableTurnProperty is the door: it refuses a write of key=value on a node
// when the node's turn, as it would stand after the write, cannot work. del is
// a delete of the key. A refusal wraps ErrTableTurnRefused and says what to
// change.
//
// What it refuses (PLAN §3):
//   - an angle that is not finite or not in 0 < |a| < π;
//   - a side that is neither lift nor set-down;
//   - a lane slot, a group or a synthetic node;
//   - a fleet that cannot turn a table;
//   - a node whose wait keys do not suit its side, or that lists no Wait key
//     (turnWaitKeysRefusal: on the lift side every wait key the location
//     lists must recognize the cart, on the set-down side none may).
func (d *Dispatcher) CheckTableTurnProperty(nodeID int64, key, value string, del bool) error {
	if !IsTableTurnProperty(key) {
		return nil
	}
	node, err := d.db.GetNode(nodeID)
	if err != nil || node == nil {
		return fmt.Errorf("%w: node %d not found (%v)", ErrTableTurnRefused, nodeID, err)
	}
	radV := d.db.GetNodeProperty(nodeID, PropTableTurnRad)
	side := d.db.GetNodeProperty(nodeID, PropTableTurnSide)
	switch {
	case key == PropTableTurnRad && del:
		radV = ""
	case key == PropTableTurnRad:
		radV = value
	case del:
		side = ""
	default:
		side = value
	}
	switch side {
	case "":
		side = TableTurnSideLift // unset is the lift side
	case TableTurnSideLift, TableTurnSideSetDown:
	default:
		return fmt.Errorf("%w: the side must be %q or %q, not %q",
			ErrTableTurnRefused, TableTurnSideLift, TableTurnSideSetDown, side)
	}
	if strings.TrimSpace(radV) == "" {
		return nil // off: nothing else to check
	}
	rad, perr := strconv.ParseFloat(strings.TrimSpace(radV), 64)
	if perr != nil || !validTurnRad(rad) {
		return fmt.Errorf("%w: the angle must be a number of radians with 0 < |angle| < π (180°), not %q",
			ErrTableTurnRefused, radV)
	}
	if err := d.turnNodeShapeRefusal(node); err != nil {
		return err
	}
	turner, _ := d.tableTurner()
	if turner == nil {
		return fmt.Errorf("%w: this fleet cannot turn a robot's table", ErrTableTurnRefused)
	}
	families, err := d.turnCarrierKeys()
	if err != nil {
		return err
	}
	return d.turnWaitKeysRefusal(map[string]string{node.Name: side}, families)
}

// turnNodeShapeRefusal refuses a node a robot does not stand at to lift or set
// down a single cart: a lane slot, a group, a synthetic node.
func (d *Dispatcher) turnNodeShapeRefusal(node *nodes.Node) error {
	if node.IsSynthetic {
		return fmt.Errorf("%w: %s is a synthetic node — a table turns at a physical spot", ErrTableTurnRefused, node.Name)
	}
	switch node.NodeTypeCode {
	case "NGRP", "LANE":
		return fmt.Errorf("%w: %s is a %s — a table turns at a physical spot", ErrTableTurnRefused, node.Name, node.NodeTypeCode)
	}
	lane, err := d.db.LaneForNode(node.ID)
	if err != nil {
		return fmt.Errorf("%w: could not read whether %s is a lane slot: %v", ErrTableTurnRefused, node.Name, err)
	}
	if lane != nil {
		return fmt.Errorf("%w: %s is a slot in lane %s — lane slots do not turn", ErrTableTurnRefused, node.Name, lane.Name)
	}
	return nil
}

// turnWaitKeysRefusal is the one check of the wait keys at turn nodes: the
// property door's for one node (CheckTableTurnProperty) and the family save's
// for every turn node (turnsAdmitWaitKey). turns is each node's side by name;
// families is the keyed families the check reads.
//
// A wait at a node should go out as the default Wait or, directly in front of
// a lift there, as the cart's family's wait key where Core's scene copy lists
// it at the location (the block builder's rule), so the check covers Wait and
// every keyed family's wait key the location lists:
//   - the lift side: each must recognize the cart, so the robot is already
//     under it when it waits there and the table turns with the cart on it;
//   - the set-down side: none may recognize: a loaded robot pausing on a
//     recognizing wait has nothing under it to find;
//   - a location that does not list Wait is refused on either side, whatever
//     family keys it lists: a cart whose family names no wait key, or one the
//     location does not list, should still send Wait on the turn wait there
//     (a set-down's turn wait, a carrying wait, always should), and RDS
//     should refuse it.
//
// A family's key the location does not list is not checked: RDS should refuse
// a block that names it there, so no robot runs it at the node.
//
// It reads the live fleet (/binCheck, one call for every node in turns), not
// Core's scene copy (scene_points), which the block builder reads to choose a
// key. It is the last check before robots are sent onto the node (no order
// re-reads a key's recognize), so it asks the fleet that runs the keys, not a
// copy that can be a sync behind it. A fleet that cannot report the keys, a
// read that fails and a location the fleet does not have all refuse: what
// cannot be checked is not admitted. The first refusal in node-name order is
// returned, wrapping ErrTableTurnRefused and naming the node, the key and what
// to change.
// PIN: TestTableTurnDoor_ChecksEveryWaitKeyTheLocationLists,
// PIN: TestTableTurnDoor_RefusesATurnThatCannotWork
func (d *Dispatcher) turnWaitKeysRefusal(turns map[string]string, families map[int64]familyKeys) error {
	if len(turns) == 0 {
		return nil
	}
	names := slices.Sorted(maps.Keys(turns))
	checker, ok := d.backend.(fleet.BinTaskChecker)
	if !ok {
		return fmt.Errorf("%w: this fleet cannot report the wait keys at %s, so whether the robot stands under the cart "+
			"there cannot be checked", ErrTableTurnRefused, strings.Join(names, ", "))
	}
	tasks, err := checker.CheckLocationTasks(names)
	if err != nil {
		return fmt.Errorf("%w: could not read the binTask keys at %s from the fleet: %v",
			ErrTableTurnRefused, strings.Join(names, ", "), err)
	}
	reported := make(map[string]fleet.LocationTasks, len(tasks))
	for _, lt := range tasks {
		reported[lt.Location] = lt
	}
	// The default first, then each family's key once, in name order, so a
	// refusal names the same key every time it is asked.
	def := seerrds.BinTaskForAction(protocol.ActionWait)
	waits := []string{def}
	for _, k := range families {
		if k.wait != "" && !slices.Contains(waits, k.wait) {
			waits = append(waits, k.wait)
		}
	}
	slices.Sort(waits[1:])
	for _, name := range names {
		lt, ok := reported[name]
		if !ok {
			return fmt.Errorf("%w: the fleet did not report bin location %s", ErrTableTurnRefused, name)
		}
		if !lt.Exists {
			return fmt.Errorf("%w: the fleet has no bin location %s", ErrTableTurnRefused, name)
		}
		if _, ok := lt.TaskArgs[def]; !ok {
			return fmt.Errorf("%w: %s has no %s key in the fleet's scene, and a turn wait there goes out as %s for "+
				"every cart whose family's wait key the location does not list, which the fleet would refuse — add a "+
				"%s key to %s in the fleet's scene first", ErrTableTurnRefused, name, def, def, def, name)
		}
		for _, key := range waits {
			raw, ok := lt.TaskArgs[key]
			if !ok {
				continue
			}
			recognizes, err := waitArgsRecognize(key, raw)
			if err != nil {
				return fmt.Errorf("%w: %s: %v", ErrTableTurnRefused, name, err)
			}
			if turns[name] == TableTurnSideSetDown && recognizes {
				return fmt.Errorf("%w: %s's %s key recognizes the cart, and %s turns the table before the set-down, "+
					"where no wait key may recognize: a loaded robot pausing on a recognizing wait has nothing under it "+
					"to find — turn before the lift here, or clear recognize on %s's %s key in the fleet's scene",
					ErrTableTurnRefused, name, key, name, name, key)
			}
			if turns[name] != TableTurnSideSetDown && !recognizes {
				return fmt.Errorf("%w: %s's %s key does not recognize the cart, so the robot is not under it when it "+
					"waits there and the table would turn with nothing on it — set recognize on %s's %s key in the "+
					"fleet's scene first", ErrTableTurnRefused, name, key, name, key)
			}
		}
	}
	return nil
}

// waitArgsRecognize reads a wait key's recognize from its parameters; a key
// that does not say does not recognize. The spelling is the scene's
// (Hopkinsville scene, 2026-11-10: {"operation":"Wait","recfile":…,
// "recognize":true}).
func waitArgsRecognize(key string, raw json.RawMessage) (bool, error) {
	var args struct {
		Recognize *bool `json:"recognize"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return false, fmt.Errorf("the %s key's parameters do not read (%v): %s", key, err, raw)
	}
	return args.Recognize != nil && *args.Recognize, nil
}

// turnCarrierKeys is the keyed families the turn checks read: the held set,
// loaded first when it never has been (a start whose load failed), so a check
// never passes a node on a set that is missing a family's key. A load that
// fails refuses: the check cannot know the keys.
// PIN: TestTableTurnDoor_ReadsTheFamiliesWhenTheSetNeverLoaded
func (d *Dispatcher) turnCarrierKeys() (map[int64]familyKeys, error) {
	if d.familyKeys.Load() == nil {
		if _, err := d.ReloadCarrierKeys(); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrTableTurnRefused, err)
		}
	}
	return d.carrierKeys(), nil
}

// turnsAdmitWaitKey is the family save's turn check (SaveCarrierFamilyKeys):
// when the family's wait key changes, every turn node (the cache, each with
// its side) goes through turnWaitKeysRefusal against the keyed families as they
// would stand after the save. A refusal names the node and the key, and the
// save writes nothing. A plant with no turn node, and a save that leaves the
// wait key as it was (a lift key changed alone), read nothing from the fleet;
// "as it was" is the held set's, which a write around the save leaves up to a
// floor pass behind the table. The fleet that cannot answer refuses the save,
// as it refuses the door: what cannot be checked is not admitted.
// PIN: TestTableTurnDoor_AFamilyWaitKeySaveIsCheckedAtEveryTurnNode,
// PIN: TestTableTurnDoor_ASaveThatCannotMoveATurnReadsNoFleet
func (d *Dispatcher) turnsAdmitWaitKey(familyID int64, wait string) error {
	d.turnMu.Lock()
	turns := make(map[string]string, len(d.turnNodes))
	for name, t := range d.turnNodes {
		turns[name] = t.side
	}
	d.turnMu.Unlock()
	if len(turns) == 0 {
		return nil
	}
	refused := func(err error) error {
		if wait == "" {
			return fmt.Errorf("the wait key is not cleared: %w", err)
		}
		return fmt.Errorf("the wait key %q is not saved: %w", wait, err)
	}
	held, err := d.turnCarrierKeys()
	if err != nil {
		return refused(err)
	}
	if held[familyID].wait == wait {
		return nil
	}
	after := make(map[int64]familyKeys, len(held)+1)
	maps.Copy(after, held)
	after[familyID] = familyKeys{wait: wait} // the check reads only the wait keys
	if err := d.turnWaitKeysRefusal(turns, after); err != nil {
		return refused(err)
	}
	return nil
}

// ── THE TURN RULE (spliceCoreWaits' second rule) ────────────────────────────
//
// A lift at a node set to turn before the lift needs the robot parked there
// with a wait in front of it, so the turn has a moment to happen in: recognize,
// drive-under and lift are one robot task with no gap. The Edge's curtain rule,
// moved to where every order passes (E/engine/release_creation.go): for each
// pickup(X) at such a node, a wait at X, kind turn, unless the step directly
// before it is already a wait at X. A swap's lift has its station wait there
// already, so a swap gets nothing. A node with no angle answers from memory.
//
// The set-down side is the same rule in front of the drop: for each dropoff(X)
// at a node set to turn before the set-down, a wait at X, kind turn, unless a
// wait at X already stands in front. The robot drives onto X loaded, the table
// turns with the cart on it, and the set-down is appended. That segment holds
// no drop, so the rules that keep a bin off an occupied node read it with the
// set-down after it (withSetDown).

// turnWaits is the turn rule's inserts for steps[from:], in plan order. A step
// before from is read only as the one a wait may already stand in front of.
func (d *Dispatcher) turnWaits(steps []resolvedStep, from int) []insertedWait {
	if !d.hasTurnNodes() {
		return nil
	}
	var out []insertedWait
	for i := from; i < len(steps); i++ {
		s := steps[i]
		t, ok := d.turnAt(s.Node)
		if !ok || !turnsBefore(s, t) {
			continue
		}
		if i > 0 && steps[i-1].Action == protocol.ActionWait && steps[i-1].Node == s.Node {
			continue // a wait at X already stands in front: the turn happens there
		}
		out = append(out, insertedWait{before: i, step: resolvedStep{
			Action: protocol.ActionWait, Node: s.Node, WaitKind: WaitKindTurn}})
	}
	return out
}

// turnsBefore reports whether a step is the one a turn node turns before: the
// lift at a lift-side node, the drop at a set-down-side node.
func turnsBefore(s resolvedStep, t turnNode) bool {
	if t.side == TableTurnSideSetDown {
		return s.Action == protocol.ActionDropoff
	}
	return s.Action == protocol.ActionPickup
}

// withSetDown is a segment as the rules that keep a bin off an occupied node
// read it (the partner rule, liftDependencies through pendingSegment; the
// vacate stamp readers): the segment, and when it ends in a turn wait at X that
// stands in front of the set-down at X, that set-down too. The segment holds no
// drop then, yet it PLACES at X: the robot drives onto X with the bin. Read only:
// what is appended is the segment alone. start is the segment's first index in
// steps (segmentStart).
func withSetDown(steps []resolvedStep, start int, seg []resolvedStep) []resolvedStep {
	n := len(seg)
	if n == 0 || start < 0 {
		return seg
	}
	last := seg[n-1]
	if last.Action != protocol.ActionWait || last.WaitKind != WaitKindTurn || last.Node == "" {
		return seg
	}
	next := start + n
	if next >= len(steps) || steps[next].Action != protocol.ActionDropoff || steps[next].Node != last.Node {
		return seg
	}
	return append(seg[:n:n], steps[next])
}

// spliceTurnWaits is the turn rule applied: the waits inserted, then the check
// that every lift at a turn node has its wait. spliceCoreWaits runs it after the
// lane rule; a home clear's trade leg, whose create skips the splice, runs it
// alone (dispatchToFleetCore).
func (d *Dispatcher) spliceTurnWaits(steps []resolvedStep) ([]resolvedStep, error) {
	return d.spliceTurnWaitsFrom(steps, 0)
}

// spliceTurnWaitsFrom is the turn rule over steps[from:]: a re-plan's span
// (repoint), the steps before it already turned or passed.
func (d *Dispatcher) spliceTurnWaitsFrom(steps []resolvedStep, from int) ([]resolvedStep, error) {
	if waits := d.turnWaits(steps, from); len(waits) > 0 {
		steps = insertWaits(steps, waits)
	}
	if err := d.assertEveryTurnHasItsWaitFrom(steps, from); err != nil {
		return nil, err
	}
	return steps, nil
}

// assertEveryTurnHasItsWait is the check after the insert: every lift at a
// lift-side turn node has a wait at that node directly in front. A plan that
// fails is a shape the rule did not foresee, and it is refused by name rather
// than shipped to lift an unturned cart.
func (d *Dispatcher) assertEveryTurnHasItsWait(steps []resolvedStep) error {
	return d.assertEveryTurnHasItsWaitFrom(steps, 0)
}

// assertEveryTurnHasItsWaitFrom is the check over steps[from:].
func (d *Dispatcher) assertEveryTurnHasItsWaitFrom(steps []resolvedStep, from int) error {
	if !d.hasTurnNodes() {
		return nil
	}
	for i := from; i < len(steps); i++ {
		s := steps[i]
		t, ok := d.turnAt(s.Node)
		if !ok || !turnsBefore(s, t) {
			continue
		}
		if i == 0 || steps[i-1].Action != protocol.ActionWait || steps[i-1].Node != s.Node {
			return fmt.Errorf("splice: step %d (%s at %s) has no wait at %s in front of it, and %s turns the "+
				"table before the %s — the table would not be turned", i, s.Action, s.Node, s.Node, s.Node, sideWords(t.side))
		}
	}
	return nil
}

// turnLiftWithSequence names the first node in a plan where a lift waits on a
// table turn: an advanced load sequence (F4c) cannot run there, because the
// lift goes out in an append and an append is never expanded.
func (d *Dispatcher) turnLiftWithSequence(steps []resolvedStep) (string, bool) {
	for _, s := range steps {
		if t, ok := d.turnAt(s.Node); ok && t.side != TableTurnSideSetDown && turnsBefore(s, t) {
			return s.Node, true
		}
	}
	return "", false
}

// hasTurnWait reports whether a plan carries a wait the turn rule inserted.
func hasTurnWait(steps []resolvedStep) bool {
	for _, s := range steps {
		if s.Action == protocol.ActionWait && s.WaitKind == WaitKindTurn {
			return true
		}
	}
	return false
}
