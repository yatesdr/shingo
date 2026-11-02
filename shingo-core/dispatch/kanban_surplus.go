package dispatch

import (
	"errors"
	"log"

	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// kanban_surplus.go — dispatch's half of the surplus arm: the moves, under the locks a
// window pull's are. The engine's keeper decides what is above its want and where it
// may go (engine/kanban_surplus.go).

// SurplusEdgeUUIDPrefix marks the arm's moves (store.SurplusEdgeUUIDPrefix, beside the
// read that finds them). The engine confirms a landed one, as it does a window pull's.
const SurplusEdgeUUIDPrefix = store.SurplusEdgeUUIDPrefix

// SurplusToWindows moves carts, in the order given, into the free windows of a live
// unloader, and returns the ids it moved and how many windows it found free.
//
// NEVER THE LAST FREE WINDOW, NOR ONE RESERVED: at most (free - 1 - reserve) moves, reserve
// being what the keeper knows is coming off the line, and a window lost to another order since it
// was read free takes one more off the count, so the process's next full has somewhere to land
// whichever window went. The windows asked of are what the caller
// read as candidates; freeWindows judges them again here.
//
// THE DECISION IS UNDER THE LOADER'S OWN LOCK (loaderLock, the one a full ask and a
// drain hold): a full ask and the arm are decided one at a time, and no second lock or
// rank is added. The keeper holds no lock of its own across this: KanbanKeeper.mu guards
// its snapshot only and is the inner of the two by rank, so taking this under it fails a
// lockcheck build. Pin: TestKanbanSurplus_TheLoaderLockOutranksTheKeepersOwn.
//
// A CART LOST TO ANOTHER MOVER IS PASSED OVER. The move takes no hold another order has
// (windowMove, no person asked), so a cart a stranger reserved since it was read refuses
// the issue and the next cart is asked.
func (d *Dispatcher) SurplusToWindows(loaderID int64, windows []*nodes.Node, carts []*bins.Bin, desc string, reserve int) ([]int64, int, error) {
	mu := d.loaderLock(loaderID)
	mu.Lock()
	defer mu.Unlock()
	free, err := d.freeWindows(windows)
	if err != nil || len(free) < 2 {
		return nil, len(free), err
	}
	caps, err := d.db.ListLoaderHomeBinTypes(loaderID)
	if err != nil {
		return nil, len(free), err
	}
	budget := len(free) - 1 - reserve
	reads := &windowReads{}
	taken := map[int64]bool{}
	var moved []int64
	for _, w := range free {
		if len(moved) >= budget {
			break
		}
		for _, c := range carts {
			if taken[c.ID] {
				continue
			}
			ok, err := d.windowAdmits(w, caps[w.ID], c, reads)
			if err != nil {
				return moved, len(free), err
			}
			if !ok {
				continue
			}
			station := d.windowStation(w)
			err = d.windowMove(SurplusEdgeUUIDPrefix, desc, c, w, station, station)
			if errors.Is(err, ErrWindowTaken) {
				budget--
				d.dbg("%s: %s was taken since it was read free", desc, w.Name)
				break
			}
			taken[c.ID] = true
			if err != nil {
				log.Printf("dispatch: %s: cart %s to %s: %v", desc, c.Label, w.Name, err)
				continue
			}
			moved = append(moved, c.ID)
			break
		}
	}
	return moved, len(free), nil
}
