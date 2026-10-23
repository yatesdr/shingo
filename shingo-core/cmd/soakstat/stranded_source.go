package main

import (
	"encoding/json"
	"fmt"

	"shingo/protocol"
)

// stranded_source.go — the releaser at a chain's end: something live bringing
// the waiter's resource (stranded.go, THE VERDICT).

// want is the resource a wait needs: an empty (or tops & bottoms) or a bin of a
// part at the place it draws from, or room in the group it drops into.
type want struct {
	kind    string // "empty", "tops_bottoms", "full" or "room"
	payload string // the part, for "full"
	place   string
}

func (w want) String() string {
	switch w.kind {
	case "full":
		return "a " + w.payload + " bin to " + w.place
	case "room":
		return "room in " + w.place
	case "tops_bottoms":
		return "tops & bottoms to " + w.place
	}
	return "an empty to " + w.place
}

// wantOf reads the resource an order waits for from its type or its steps. A
// leg whose steps open on a wait is waiting on its pair, and takes its live
// sibling's resource.
func (re *runEnd) wantOf(o *activeOrder) (want, bool) {
	switch o.typ {
	case string(protocol.OrderTypeRetrieveEmpty):
		return want{kind: "empty", place: o.src}, o.src != ""
	case string(protocol.OrderTypeRetrieveTopsAndBottoms):
		// SEND BACK's pull: a tops & bottoms cart at its source.
		return want{kind: "tops_bottoms", place: o.src}, o.src != ""
	case string(protocol.OrderTypeRetrieve):
		return want{kind: "full", payload: o.payload, place: o.src}, o.src != ""
	}
	steps := parseSteps(o.steps)
	if len(steps) == 0 {
		return want{}, false
	}
	if steps[0].Action == protocol.ActionWait && o.sibling != "" {
		if sib := re.byUUID(o.sibling); sib != nil && sib.id != o.id {
			if ss := parseSteps(sib.steps); len(ss) > 0 && ss[0].Action != protocol.ActionWait {
				return re.wantOf(sib)
			}
		}
	}
	for _, s := range steps {
		switch s.Action {
		case protocol.ActionPickup:
			if s.Empty {
				return want{kind: "empty", place: s.Node}, true
			}
			return want{kind: "full", payload: o.payload, place: s.Node}, true
		case protocol.ActionDropoff:
			return want{kind: "room", place: firstNonEmpty(s.Group, s.Node)}, true
		}
	}
	return want{}, false
}

// bringer is the releaser read at a chain's end: a live order bringing the
// waiter's resource, or work near it when the resource cannot be read.
func (re *runEnd) bringer(near nearProgress) func(*activeOrder) (string, bool) {
	return func(o *activeOrder) (string, bool) {
		w, ok := re.wantOf(o)
		if !ok {
			if near.moved(o) {
				return "resource unread; work progressing at its nodes or groups", true
			}
			return fmt.Sprintf("resource unread, and no work progressed at its nodes or their groups in %d sim-min", strandedQuiet), false
		}
		for _, id := range re.ids {
			h := re.byID[id]
			if h.id == o.id || h.stranded || protocol.Status(h.status).IsPreDispatch() {
				continue
			}
			if brings(h, w, near) {
				return fmt.Sprintf("%s: order %d (%s) is bringing it", w, h.id, h.status), true
			}
		}
		return fmt.Sprintf("but nothing live is bringing %s", w), false
	}
}

// brings reports whether a live order puts w's resource where w needs it: an
// empty or a bin of the part delivered into the place, or a bin picked up out
// of the group that needs room.
func brings(h *activeOrder, w want, near nearProgress) bool {
	within := func(node string) bool {
		for _, p := range near.placesOf(node) {
			if p == w.place {
				return true
			}
		}
		return false
	}
	switch w.kind {
	case "room":
		return h.src != "" && within(h.src)
	case "empty", "tops_bottoms":
		// An order carrying no part into the place brings an empty; its height
		// is not read here, so it is taken as bringing tops & bottoms too.
		return h.carries == "" && within(dropOf(h))
	}
	return h.carries == w.payload && within(dropOf(h))
}

// dropOf is where an order puts its bin: its last dropoff step's group or node,
// else its delivery node.
func dropOf(h *activeOrder) string {
	steps := parseSteps(h.steps)
	for i := len(steps) - 1; i >= 0; i-- {
		if steps[i].Action == protocol.ActionDropoff {
			return firstNonEmpty(steps[i].Group, steps[i].Node)
		}
	}
	return h.dst
}

type stepRow struct {
	Action string `json:"action"`
	Node   string `json:"node"`
	Group  string `json:"group"`
	Empty  bool   `json:"empty"`
}

// parseSteps reads steps_json; unparseable steps are none.
func parseSteps(stepsJSON string) []stepRow {
	if stepsJSON == "" {
		return nil
	}
	var steps []stepRow
	if json.Unmarshal([]byte(stepsJSON), &steps) != nil {
		return nil
	}
	return steps
}

// byUUID finds an active order by its edge_uuid.
func (re *runEnd) byUUID(uuid string) *activeOrder {
	for _, o := range re.byID {
		if o.uuid == uuid {
			return o
		}
	}
	return nil
}
