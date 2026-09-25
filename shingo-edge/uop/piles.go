// piles.go — the lineside pile verbs outside the capture and the tick.
//
// Every write to node_lineside_bucket marks the written key's level dirty, and
// the flush sends each dirty key's level as read at flush. The capture and the
// tick mark through their own verbs; the cutover's strand, the admin Clear and
// a process delete write the rows themselves (in the store, in one statement or
// one transaction) and hand the keys here. One writer set, one rule.
package uop

import (
	"fmt"
	"log"

	"shingo/protocol"
	"shingoedge/store/lineside"
)

// PilesChanged marks each key's level dirty and flushes. The caller has
// already written the rows (a deleted row's level goes out as 0).
func (m *Mutator) PilesChanged(keys ...lineside.Key) {
	for _, k := range keys {
		m.acc.markBucket(k.NodeID, k.CoreNodeName, k.PayloadCode, protocol.LinesideBucketState(k.State), 0)
	}
	m.acc.flush()
}

// ResendLevels is the boot resend: every pile row's key is marked dirty and
// flushed, so Core's mirror is re-seeded from the Edge unconditionally. It is
// idempotent under Core's seq guard (each level carries a fresh seq), which is
// why there is no probe of what Core holds first. Returns how many keys it
// marked.
func (m *Mutator) ResendLevels() (int, error) {
	keys, err := m.buckets.ListLinesidePileKeys()
	if err != nil {
		return 0, fmt.Errorf("list lineside piles: %w", err)
	}
	m.PilesChanged(keys...)
	log.Printf("uop: resent %d lineside pile level(s)", len(keys))
	return len(keys), nil
}
