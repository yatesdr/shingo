package dispatch

import (
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// loader_window.go — a loader window as Core's own pulls judge it.
//
// Two pulls send a named cart to a window Core chose: stage 2's
// (engine/stage2_pull.go: a bare cart from the wait group) and the drain
// (DrainLoader: a part-less cart from an unloader's inbound source). Whether a
// window is free and whether it admits a cart is one predicate each, here, so
// the two pulls cannot disagree about a window. They began as stage 2's own
// (freeStage2Windows, windowAdmits) and moved down when the drain became their
// second caller.

// FreeWindows returns, in the order given, the windows a pull may send a cart
// to now: enabled, no bin on it, no non-terminal order delivering there, and
// no other order's slot reservation. Core's own truth, no Edge heuristic.
//
// ANOTHER ORDER'S SLOT RESERVATION: the pull's move claims a storage-classed
// window through ReserveStorageDropoff, which refuses a window somebody else
// has reserved.
//
// Three reads per enabled window (its bins, the orders delivering to it, its
// slot reservations), none for a disabled one.
func (d *Dispatcher) FreeWindows(windows []*nodes.Node) ([]*nodes.Node, error) {
	var free []*nodes.Node
	for _, w := range windows {
		if !w.Enabled {
			continue
		}
		n, err := d.db.CountBinsByNode(w.ID)
		if err != nil {
			return nil, err
		}
		inbound, err := d.db.CountActiveOrdersByDeliveryNode(w.Name)
		if err != nil {
			return nil, err
		}
		if n != 0 || inbound != 0 {
			continue
		}
		spoken, err := d.db.SlotSpokenForByStranger(w.ID, 0)
		if err != nil {
			return nil, err
		}
		if !spoken {
			free = append(free, w)
		}
	}
	return free, nil
}

// WindowReads is what one pull has read about its windows and carts, so each
// is read once however many window and cart pairs are asked about: a window's
// Allowed Bin Types (once per window; it was once per pair) and a cart type's
// row. The zero value is ready; it lives for one pull.
type WindowReads struct {
	allowed map[int64][]*bins.BinType // by window node id
	types   map[int64]*bins.BinType   // by bin type id
}

// WindowAdmits applies both carrier restrictions a cart meets at a window —
// the node's Allowed Bin Types and the window's own capability list
// (capCodes: ListLoaderHomeBinTypes; none takes anything) — with the marker
// admitted wherever its carrier is.
func (d *Dispatcher) WindowAdmits(w *nodes.Node, capCodes []string, c *bins.Bin, reads *WindowReads) (bool, error) {
	cart, err := reads.binType(d, c.BinTypeID)
	if err != nil {
		return false, err
	}
	allowed, err := reads.allowedAt(d, w.ID)
	if err != nil {
		return false, err
	}
	if !bins.TypeAdmits(allowed, cart.ID, cart.BareOf) {
		return false, nil
	}
	if len(capCodes) == 0 {
		return true, nil
	}
	carrierCode := ""
	if cart.BareOf != nil {
		carrier, err := reads.binType(d, *cart.BareOf)
		if err != nil {
			return false, err
		}
		carrierCode = carrier.Code
	}
	for _, code := range capCodes {
		if code == cart.Code || (carrierCode != "" && code == carrierCode) {
			return true, nil
		}
	}
	return false, nil
}

// binType is a bin type's row, read once per pull.
func (r *WindowReads) binType(d *Dispatcher, id int64) (*bins.BinType, error) {
	if bt, ok := r.types[id]; ok {
		return bt, nil
	}
	bt, err := d.db.GetBinType(id)
	if err != nil {
		return nil, err
	}
	if r.types == nil {
		r.types = map[int64]*bins.BinType{}
	}
	r.types[id] = bt
	return bt, nil
}

// allowedAt is a window's Allowed Bin Types, read once per pull.
func (r *WindowReads) allowedAt(d *Dispatcher, windowID int64) ([]*bins.BinType, error) {
	if bts, ok := r.allowed[windowID]; ok {
		return bts, nil
	}
	bts, err := d.db.GetEffectiveBinTypes(windowID)
	if err != nil {
		return nil, err
	}
	if r.allowed == nil {
		r.allowed = map[int64][]*bins.BinType{}
	}
	r.allowed[windowID] = bts
	return bts, nil
}

// WindowStation is the Edge station a pull's move belongs to, so the delivered
// notice reaches the board holding the window: the window's one effective
// station, else the plant's one Edge, else none (the move still runs; the Edge
// learns of the cart from the bin on the window).
func (d *Dispatcher) WindowStation(w *nodes.Node) string {
	if sts, err := d.db.GetEffectiveStations(w.ID); err == nil && len(sts) == 1 {
		return sts[0]
	}
	if edges, err := d.db.ListEdges(); err == nil && len(edges) == 1 {
		return edges[0].StationID
	}
	return ""
}
