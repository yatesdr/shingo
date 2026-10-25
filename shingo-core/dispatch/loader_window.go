package dispatch

import (
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingocore/dispatch/plan"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// loader_window.go — Core's two window pulls: a named cart into a loader
// window Core chose.
//
// Stage 2's pull (PullStage2: a bare cart from a two-stage unloader's wait
// group) and the drain (DrainLoader: a part-less cart from an unloader's
// inbound source) send their carts the same way (pullInto), one move per free
// window (windowMove). Whether a window is free and whether it admits a cart
// is one predicate each, so the two pulls cannot disagree about a window. The
// engine keeps their triggers, the goroutines they run on and the confirm of a
// landed move (engine/window_pulls.go).

// Stage2EdgeUUIDPrefix marks stage 2's pull and DrainEdgeUUIDPrefix the
// drain's, beside core-l1- and core-u1- (the loader loop's asks) and core-mnt-
// (the level keeper). The engine confirms a landed one of either: there is no
// receipt to wait for.
const (
	Stage2EdgeUUIDPrefix = "core-s2-"
	DrainEdgeUUIDPrefix  = "core-dr-"
)

// PullStage2 moves bare carts from a pair's wait group into its free stage-2
// windows, one move per free window, and returns how many it made. A pair in
// direct mode (stage 2 has no inbound source) never pulls.
//
// The windows are stage 2's homes, judged by freeWindows. The carts are the
// wait group's bare carts, unclaimed, sourceable and reachable, oldest arrival
// first (bins.ListBareInGroup), each taken only by a window that admits it. A
// wait group is an ordinary group that several pairs may name: two pairs with
// different cart types share it without taking each other's carts, and two
// with the same type share it first come first served.
//
// THE DECISION IS UNDER STAGE 2'S OWN LOADER LOCK (loaderLock), the one its
// full asks and a drain hold: a pull and a full ask at stage 2 are decided one
// at a time, and a burst of triggers makes one move per free window.
func (d *Dispatcher) PullStage2(stage1ID int64) (int, error) {
	one, two, err := d.pullPair(stage1ID)
	if err != nil || two == nil || two.InboundSource == "" {
		return 0, err
	}
	mu := d.loaderLock(two.ID)
	mu.Lock()
	defer mu.Unlock()
	group, err := d.db.GetNodeByDotName(two.InboundSource)
	if err != nil {
		return 0, fmt.Errorf("stage 2 of %s pulls from %q: %w", one.Name, two.InboundSource, err)
	}
	free, err := d.stage2Windows(two)
	if err != nil || len(free) == 0 {
		return 0, err
	}
	carts, err := d.db.ListBareCartsInGroup(group.ID)
	if err != nil || len(carts) == 0 {
		return 0, err
	}
	caps, err := d.db.ListLoaderHomeBinTypes(two.ID)
	if err != nil {
		return 0, err
	}
	return d.pullInto(free, carts, caps, Stage2EdgeUUIDPrefix,
		"stage 2 pull for "+strings.TrimSuffix(one.Name, " · stage 1"))
}

// pullPair returns a live pair by its stage-1 id, or nils.
func (d *Dispatcher) pullPair(stage1ID int64) (*loaders.Loader, *loaders.Loader, error) {
	one, err := d.db.GetLoader(stage1ID)
	if err != nil || one == nil || one.ArchivedAt != nil || one.SecondStageLoaderID == nil {
		return nil, nil, err
	}
	two, err := d.db.GetLoader(*one.SecondStageLoaderID)
	if err != nil || two == nil || two.ArchivedAt != nil {
		return nil, nil, err
	}
	return one, two, nil
}

// stage2Windows returns stage 2's free windows: its homes, as nodes, judged by
// freeWindows.
func (d *Dispatcher) stage2Windows(two *loaders.Loader) ([]*nodes.Node, error) {
	homes, err := d.db.ListLoaderHomes(two.ID)
	if err != nil {
		return nil, err
	}
	windows := make([]*nodes.Node, 0, len(homes))
	for _, h := range homes {
		w, err := d.db.GetNode(h.PositionNodeID)
		if err != nil {
			return nil, err
		}
		windows = append(windows, w)
	}
	return d.freeWindows(windows)
}

// pullInto sends carts into the free windows, in the order given: each window
// the first cart it admits, each cart at most once. It returns how many moves
// it made. A cart that could not go (lost to another mover, its plan refused)
// leaves the window to the next cart; a window lost to another order since it
// was read free is no move and the loop's next window, where the cart may go.
func (d *Dispatcher) pullInto(free []*nodes.Node, carts []*bins.Bin, caps map[int64][]string, prefix, desc string) (int, error) {
	reads := &windowReads{}
	taken := map[int64]bool{}
	moved := 0
	for _, w := range free {
		for _, c := range carts {
			if taken[c.ID] {
				continue
			}
			ok, err := d.windowAdmits(w, caps[w.ID], c, reads)
			if err != nil {
				return moved, err
			}
			if !ok {
				continue
			}
			err = d.windowMove(prefix, desc, c, w)
			if errors.Is(err, errWindowTaken) {
				d.dbg("%s: %s was taken since it was read free", desc, w.Name)
				break
			}
			taken[c.ID] = true
			if err != nil {
				log.Printf("dispatch: %s: cart %s to %s: %v", desc, c.Label, w.Name, err)
				continue
			}
			moved++
			break
		}
	}
	return moved, nil
}

// windowMove issues one move of cart c into window w, for the cart and holding
// the window from its issue (Need.Cart, Need.Window), and acquires it once. The
// plan is the bin move's (PlanMove: the cart at its node, the window's capacity
// and carrier rule). A window pull is Core's own errand, never a person's, so
// it takes no hold another order has: a stranger's hold on the cart refuses the
// issue (the pulls' reads of carts pass over a held one, so only a race meets
// it), and so does another order's on the window (errWindowTaken). It asks no
// dig verdict: both pulls read only reachable carts. Station-unaware like every
// Core-born move; its material notices project to the window's Edge
// (Need.Projects), which binds the cart when it lands.
//
// The row is whole at its INSERT, as a Core ask's is, so it takes the same one
// acquire (acquireCoreAsk), under the loader's lock its pull holds.
func (d *Dispatcher) windowMove(prefix, desc string, c *bins.Bin, w *nodes.Node) error {
	if c.NodeID == nil {
		return errors.New("the cart is at no node")
	}
	at, err := d.db.GetNode(*c.NodeID)
	if err != nil {
		return err
	}
	p := d.PlanMove(plan.Given{Bin: c, Source: at, Dest: w})
	if p.Refused != nil {
		return fmt.Errorf("the move's plan refused it (rule %d): %v", p.Refused.Rule, p.Refused.Err)
	}
	station := d.windowStation(w)
	order, _, err := d.IssueOrder(Need{
		Type:     OrderTypeMove,
		Station:  station,
		Ref:      prefix + uuid.New().String(),
		Quantity: 1,
		Desc:     desc,
		// No place asked for material: the loader is clearing a cart it was set
		// up to clear, so there is no episode, and blank would read orphan.
		OriginClass: protocol.OriginClassNoDemand,
		Projects:    station,
		Cart:        c.ID,
		Window:      w,
	}, p)
	if err != nil {
		return err
	}
	d.acquireCoreAsk(order)
	return nil
}

// freeWindows returns, in the order given, the windows a pull may send a cart
// to now: enabled, no bin on it, no carrier coming to it, and no other order's
// slot reservation. Core's own truth, no Edge heuristic.
//
// NO CARRIER COMING is rule 8's one spelling (orders.CarrierComingTo:
// protocol.OrderPlacesBinAt over the live rows whose delivery node or steps
// name the window), so a leg that sets a carrier down at the window before its
// last act holds it, and a leg that only lifts from it does not. A pull is no
// order yet, so it asks as the zero order and excludes nobody. Pinned by
// TestComingVectors_TheWindowReadersAgree.
//
// ANOTHER ORDER'S SLOT RESERVATION: a pull's move holds its window from its
// issue (windowMove), so a window one pull's move is on its way to is not
// another's, nor a full ask's (plan.LoaderWindows reads the same row).
//
// Three reads per enabled window (its bins, the live rows naming it, its slot
// reservations), none for a disabled one.
func (d *Dispatcher) freeWindows(windows []*nodes.Node) ([]*nodes.Node, error) {
	var free []*nodes.Node
	for _, w := range windows {
		if !w.Enabled {
			continue
		}
		n, err := d.db.CountBinsByNode(w.ID)
		if err != nil {
			return nil, err
		}
		coming, err := d.db.CarrierComingTo(w.Name, &orders.Order{})
		if err != nil {
			return nil, err
		}
		if n != 0 || coming {
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

// windowReads is what one pull has read about its windows and carts, so each
// is read once however many window and cart pairs are asked about: a window's
// Allowed Bin Types (once per window; it was once per pair) and a cart type's
// row. The zero value is ready; it lives for one pull.
type windowReads struct {
	allowed map[int64][]*bins.BinType // by window node id
	types   map[int64]*bins.BinType   // by bin type id
}

// windowAdmits applies both carrier restrictions a cart meets at a window —
// the node's Allowed Bin Types and the window's own capability list
// (capCodes: ListLoaderHomeBinTypes; none takes anything) — with the marker
// admitted wherever its carrier is.
func (d *Dispatcher) windowAdmits(w *nodes.Node, capCodes []string, c *bins.Bin, reads *windowReads) (bool, error) {
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
func (r *windowReads) binType(d *Dispatcher, id int64) (*bins.BinType, error) {
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
func (r *windowReads) allowedAt(d *Dispatcher, windowID int64) ([]*bins.BinType, error) {
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

// windowStation is the Edge station a pull's move belongs to, so the delivered
// notice reaches the board holding the window: the window's one effective
// station, else the plant's one Edge, else none (the move still runs; the Edge
// learns of the cart from the bin on the window).
func (d *Dispatcher) windowStation(w *nodes.Node) string {
	if sts, err := d.db.GetEffectiveStations(w.ID); err == nil && len(sts) == 1 {
		return sts[0]
	}
	if edges, err := d.db.ListEdges(); err == nil && len(edges) == 1 {
		return edges[0].StationID
	}
	return ""
}
