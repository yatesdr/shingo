package plan

import (
	"database/sql"
	"errors"

	"shingocore/store/loaders"
)

// LoaderWindowsDB is what the loader window choice reads beyond the capacity
// gate: whether another order's slot reservation speaks for a window, and
// whether a carrier has already been asked for at it.
type LoaderWindowsDB interface {
	CapacityDB
	SlotSpokenForByStranger(nodeID, ownerID int64) (bool, error)
	CountLiveCarrierRequestsByDeliveryNode(deliveryNode, intent string) (int, error)
}

// LoaderWindows is a loader replenishment's window choice (B3): the windows to
// ask at, in target order, and why each window passed over was passed over.
type LoaderWindows struct {
	// Chosen is where one carrier each is asked for.
	Chosen []loaders.Target
	// HeldBy names each window that cannot take a carrier now: the capacity
	// verdict's cause, "window-reserved", "window-order-open" or
	// "window-check-failed".
	HeldBy map[string]string
	// Targets and Budget are the loader's delivery targets and never-2N budget,
	// for the decision record.
	Targets, Budget int
	// ReadErrs are the reservation and open-ask reads that failed, by window,
	// for the caller's debug line. Each such window is held, never chosen.
	ReadErrs map[string]error
}

// LoaderWindows chooses where a loader's replenishment asks, up to want carriers:
// the loader's delivery targets in order, each gated at decision time by the
// capacity verdict, by another order's slot reservation, and by whether what
// this ask asks for (intent: an empty or a full) is already asked for there. It
// writes nothing (the dispatcher births one order per chosen window).
//
// ANOTHER ORDER'S SLOT RESERVATION, which the capacity verdict does not read: a
// pull's claim on a storage-classed window would be refused for as long as the
// holder holds it. No pull exists yet, so nobody is exempt (owner 0). A name
// that resolves to no node passes, the answer the capacity verdict gives it, so
// the dispatch produces the real error. (main's loader pull window, rebase 4.)
//
// THE TARGET LIST IS THE BOUND. DeliveryTargets returns a budget equal to its
// target count in every shape that exists — spread gives every window and the
// window count, funnel and dedicated each give one node and 1 — so walking the
// targets caps the ask at the budget; want stops it early when the loop needs
// fewer, and the per-window checks remove the ones that cannot take one now.
//
// ONE ORDER PER WINDOW, FROM ANY ASKER. The open-ask count, of the ask's own
// intent, is asked from any origin on purpose: two payloads at one shared-window
// loader are two episodes that cannot see each other, so an episode-scoped
// question would let both put a carrier on the same window. For an empty it
// counts ASKS, not arrivals: a swap's evac leg names a home as its delivery node
// but never asked for a carrier, and counting it deadlocked SMN_030 for 8h57m on
// 2026-08-05. For a full the intent is shared with a complex order's parent, so
// an evac bringing a full does count (see
// orders.CountLiveCarrierRequestsByDeliveryNode). Arrivals are the capacity
// verdict's in-flight arm. Unreadable fails closed, like every occupancy question.
func (p *Planner) LoaderWindows(in loaders.DeliveryTargetsInput, want int, intent string) LoaderWindows {
	defer observe(p.db, "loader-windows")()
	targets, budget := loaders.DeliveryTargets(in)
	w := LoaderWindows{HeldBy: map[string]string{}, Targets: len(targets), Budget: budget}
	if len(targets) == 0 || budget <= 0 {
		return w
	}
	for _, t := range targets {
		if len(w.Chosen) >= want {
			break
		}
		// No order of ours exists yet to exclude: that is the whole difference
		// between deciding and retrying.
		if blocked, v := DropoffCapacity(p.db, t.NodeName, 0); blocked {
			w.HeldBy[t.NodeName] = string(v.Cause)
			continue
		}
		if held, err := p.windowReserved(t.NodeName); err != nil || held {
			if err != nil {
				w.heldUnread(t.NodeName, err)
			} else {
				w.HeldBy[t.NodeName] = "window-reserved"
			}
			continue
		}
		live, err := p.db.CountLiveCarrierRequestsByDeliveryNode(t.NodeName, intent)
		if err != nil {
			w.heldUnread(t.NodeName, err)
			continue
		}
		if live > 0 {
			w.HeldBy[t.NodeName] = "window-order-open"
			continue
		}
		w.Chosen = append(w.Chosen, t)
	}
	return w
}

// windowReserved reports whether another order's slot reservation speaks for the
// window: one node read and one reservation read, as the window gate has made
// them since slot reservations were read by every chooser.
func (p *Planner) windowReserved(name string) (bool, error) {
	node, err := p.db.GetNodeByDotName(name)
	switch {
	case errors.Is(err, sql.ErrNoRows), err == nil && node == nil:
		return false, nil
	case err != nil:
		return false, err
	}
	return p.db.SlotSpokenForByStranger(node.ID, 0)
}

// heldUnread holds a window whose occupancy could not be read: fail closed.
func (w *LoaderWindows) heldUnread(name string, err error) {
	w.HeldBy[name] = "window-check-failed"
	if w.ReadErrs == nil {
		w.ReadErrs = map[string]error{}
	}
	w.ReadErrs[name] = err
}
