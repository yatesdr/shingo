package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"

	"shingocore/fleet"
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

// setDownTurnBuilt is whether the set-down side is built (the plan's phase 6).
// Until it is, the door refuses the side outright: a wait in front of a drop
// changes what a swap's supply segment holds, and the rules that keep a supply
// off an occupied line must read it first (SYNTH-round3 §6.2).
const setDownTurnBuilt = false

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
//   - a side that is neither lift nor set-down; the set-down side at all until
//     it is built;
//   - a lane slot, a group or a synthetic node;
//   - a fleet that cannot turn a table;
//   - the lift side on a node whose fleet Wait key does not recognize the cart
//     (the robot must already be under it at the wait), or that has none;
//   - the set-down side on a node whose Wait key recognizes: a loaded robot
//     pausing on a recognizing Wait has nothing under it to find.
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
	case "", TableTurnSideLift:
	case TableTurnSideSetDown:
		if !setDownTurnBuilt {
			return fmt.Errorf("%w: turning the table before the set-down is not built yet — set the side to %q",
				ErrTableTurnRefused, TableTurnSideLift)
		}
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
	recognizes, err := d.waitKeyRecognizes(node.Name)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrTableTurnRefused, err)
	}
	if side == TableTurnSideSetDown {
		if recognizes {
			return fmt.Errorf("%w: %s's Wait key recognizes the cart, so a loaded robot pausing there before the "+
				"set-down has nothing under it to find — turn before the lift here, or give the node a Wait key "+
				"that does not recognize", ErrTableTurnRefused, node.Name)
		}
		return nil
	}
	if !recognizes {
		return fmt.Errorf("%w: %s's Wait key does not recognize the cart, so the robot is not under it when it "+
			"waits there and the table would turn with nothing on it — set recognize on the node's Wait key "+
			"in the fleet's scene first", ErrTableTurnRefused, node.Name)
	}
	return nil
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

// waitKeyRecognizes reads the fleet's Wait key at a location and reports
// whether it recognizes the cart. A location with no Wait key, a fleet that
// cannot say, and a read that fails are errors: the door cannot check, so it
// refuses.
func (d *Dispatcher) waitKeyRecognizes(location string) (bool, error) {
	checker, ok := d.backend.(fleet.BinTaskChecker)
	if !ok {
		return false, fmt.Errorf("this fleet cannot report %s's Wait key, so whether the robot stands under the cart there cannot be checked", location)
	}
	tasks, err := checker.CheckLocationTasks([]string{location})
	if err != nil {
		return false, fmt.Errorf("could not read %s's binTask keys from the fleet: %v", location, err)
	}
	for _, lt := range tasks {
		if lt.Location != location {
			continue
		}
		if !lt.Exists {
			return false, fmt.Errorf("the fleet has no bin location %s", location)
		}
		raw, ok := lt.TaskArgs["Wait"]
		if !ok {
			return false, fmt.Errorf("%s has no Wait key in the fleet's scene", location)
		}
		return waitArgsRecognize(raw)
	}
	return false, fmt.Errorf("the fleet did not report bin location %s", location)
}

// waitArgsRecognize reads a Wait key's recognize. The spelling is the scene's
// (Hopkinsville scene, 2026-11-10: {"operation":"Wait","recfile":…,
// "recognize":true}).
func waitArgsRecognize(raw json.RawMessage) (bool, error) {
	var args struct {
		Recognize *bool `json:"recognize"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return false, fmt.Errorf("the Wait key's parameters do not read (%v): %s", err, raw)
	}
	return args.Recognize != nil && *args.Recognize, nil
}
