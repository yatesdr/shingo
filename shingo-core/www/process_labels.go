package www

import (
	"log"
	"strings"

	"shingocore/domain"
)

// processLabels names the process an order or a carrier belongs to, for the
// boards' Process column: one read of the mirror per page (ProcessesByNode),
// and one rule for every page that shows the column, so the orders board, the
// home page and the bins page cannot answer differently.
type processLabels map[string][]string

// processLabelsFor reads the mirror once. A failed read labels nothing: an
// empty column, never a guess.
func (h *Handlers) processLabelsFor() processLabels {
	byNode, err := h.engine.NodeService().ProcessesByNode()
	if err != nil {
		log.Printf("process labels: %v", err)
		return nil
	}
	return processLabels(byNode)
}

// node is the process (or processes, "A / B" for a shared node) that names a
// Core node; "" for a node no process names, such as a supermarket slot.
func (p processLabels) node(name string) string {
	return strings.Join(p[name], " / ")
}

// order is the process an order serves: its line node when it has one, else
// where it delivers, else where it picks up. A loader refill to a window names
// the loader's process; an order between two supermarket slots names none.
func (p processLabels) order(o *domain.Order) string {
	for _, n := range []string{o.ProcessNode, o.DeliveryNode, o.SourceNode} {
		if label := p.node(n); label != "" {
			return label
		}
	}
	return ""
}

// orders labels a page's orders by id, for a template's {{index $.OrderProcess .ID}}.
func (p processLabels) orders(list []*domain.Order) map[int64]string {
	out := make(map[int64]string, len(list))
	for _, o := range list {
		out[o.ID] = p.order(o)
	}
	return out
}
