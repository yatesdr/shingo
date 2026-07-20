// Package scenesim is the Level-2 scene-physics harness for the lane seam. Where
// the lifecycle simulator (fleet/simulator) hand-drives order state, this layer
// moves robot tokens over a real plant scene and completes an order's blocks only
// when the robot has physically earned them — so single-file lanes, occupancy,
// and the deadlocks the lane seam exists to kill become representable.
//
// S0 (this slice) is the skeleton: scene loader from Core's real node model,
// robot tokens with coarse travel, a tick loop, node/lane occupancy, single-file
// lanes as ordered stacks (no passing, mouth-only entry, robots CAN be trapped),
// and the first two invariant checkers (mode purity, no deadlock).
//
// It consumes the REAL wire types (fleet.CreateOrderRequest / OrderBlock) and the
// REAL node model (domain.Node), so a scene is a plant's actual geometry, not a
// toy. It never modifies production code.
//
// Fidelity note: travel time is deliberately COARSE — a fixed number of ticks per
// hop, not distance-in-seconds. The harness is trustworthy for ORDERING and
// OCCUPANCY (which robot is where, in what sequence), never for wall-clock timing.
package scenesim

import (
	"fmt"
	"sort"

	"shingo/protocol"
	"shingocore/domain"
)

// Kind classifies a scene node for the physics. Derived from the node's type code
// in the real config; unknown/other codes are treated as plain positions.
type Kind int

const (
	KindPosition Kind = iota // a plain node a robot can occupy (STOR, LINE, staging, …)
	KindGroup                // NGRP — a synthetic grouping; not itself occupiable
	KindLane                 // LANE — a depth-ordered single-file stack of slots
	KindSlot                 // a direct child of a lane; occupiable, has a depth
)

// Node is a scene node keyed by NAME (the same string fleet blocks address via
// OrderBlock.Location).
type Node struct {
	Name   string
	Kind   Kind
	Parent string // parent node name ("" for roots)
	Depth  int    // meaningful for slots (0 = shallowest = nearest the mouth)
}

// Lane is a single-file stack: its slots ordered shallow→deep. Index 0 is the
// mouth slot; entry and exit are only through the mouth end.
type Lane struct {
	Name  string
	Slots []string // slot names, shallow (index 0 = mouth) → deep
}

// Scene is the loaded plant geometry.
type Scene struct {
	nodes    map[string]*Node
	lanes    map[string]*Lane  // by lane name
	slotLane map[string]string // slot name → owning lane name
}

// LoadScene builds a Scene from Core's real node config. A LANE node's slots are
// its direct children ordered by depth; a slot with no depth sorts as depth 0.
// Names must be unique (they are the wire address space); a duplicate is an error.
func LoadScene(nodes []domain.Node) (*Scene, error) {
	s := &Scene{
		nodes:    make(map[string]*Node, len(nodes)),
		lanes:    make(map[string]*Lane),
		slotLane: make(map[string]string),
	}

	// Index parents by ID so a child can resolve its parent's name/kind.
	nameByID := make(map[int64]string, len(nodes))
	for i := range nodes {
		nameByID[nodes[i].ID] = nodes[i].Name
	}

	for i := range nodes {
		n := &nodes[i]
		if n.Name == "" {
			return nil, fmt.Errorf("scene: node id %d has no name", n.ID)
		}
		if _, dup := s.nodes[n.Name]; dup {
			return nil, fmt.Errorf("scene: duplicate node name %q", n.Name)
		}
		parent := ""
		if n.ParentID != nil {
			parent = nameByID[*n.ParentID]
		}
		depth := 0
		if n.Depth != nil {
			depth = *n.Depth
		}
		s.nodes[n.Name] = &Node{Name: n.Name, Kind: kindOf(n), Parent: parent, Depth: depth}
	}

	// A node is a SLOT if its parent is a LANE. Reclassify + collect per lane.
	laneSlots := map[string][]*Node{}
	for _, n := range s.nodes {
		if n.Parent == "" {
			continue
		}
		if p := s.nodes[n.Parent]; p != nil && p.Kind == KindLane {
			n.Kind = KindSlot
			laneSlots[n.Parent] = append(laneSlots[n.Parent], n)
		}
	}
	for laneName, slots := range laneSlots {
		sort.SliceStable(slots, func(i, j int) bool { return slots[i].Depth < slots[j].Depth })
		lane := &Lane{Name: laneName}
		for _, sl := range slots {
			lane.Slots = append(lane.Slots, sl.Name)
			s.slotLane[sl.Name] = laneName
		}
		s.lanes[laneName] = lane
	}
	return s, nil
}

func kindOf(n *domain.Node) Kind {
	switch n.NodeTypeCode {
	case protocol.NodeClassLANE:
		return KindLane
	case protocol.NodeClassNGRP:
		return KindGroup
	default:
		return KindPosition
	}
}

// Node returns the scene node by name, or nil.
func (s *Scene) Node(name string) *Node { return s.nodes[name] }

// LaneForNode returns the lane a node belongs to — the owning lane if the node is
// a slot, the lane itself if the node is a lane, else "". This mirrors Core's
// one-hop laneForNode walk (a slot's parent is its lane).
func (s *Scene) LaneForNode(name string) string {
	if _, ok := s.lanes[name]; ok {
		return name
	}
	return s.slotLane[name]
}

// Lane returns the lane by name, or nil.
func (s *Scene) Lane(name string) *Lane { return s.lanes[name] }

// SlotDepth returns a slot's index within its lane (0 = mouth) and whether the
// node is a slot at all.
func (s *Scene) SlotDepth(name string) (int, bool) {
	lane := s.slotLane[name]
	if lane == "" {
		return 0, false
	}
	for i, sl := range s.lanes[lane].Slots {
		if sl == name {
			return i, true
		}
	}
	return 0, false
}

// LaneNames returns every lane's name (sorted, for stable iteration).
func (s *Scene) LaneNames() []string {
	out := make([]string, 0, len(s.lanes))
	for name := range s.lanes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
