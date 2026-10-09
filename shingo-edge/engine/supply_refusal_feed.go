package engine

import (
	"fmt"

	"shingo/protocol"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// supply_refusal_feed.go — the FeedRefusals digest this Edge quotes on every
// heartbeat, and the per-field merge ApplySupplyRefusalSnapshot runs when
// Core's open set differs from it.
//
// Refusals are the one feed both sides write: the loader's Edge opens and
// closes a card's refusal, a cell's Edge answers it, and Core records and
// relays both. So the Edge digests its own table rather than quoting a digest
// Core sent, and a snapshot from Core is merged, not copied.

// refusalOwners is what this Edge owns in the refusal set, read once per
// snapshot apply.
type refusalOwners struct {
	// windows are the Core node names of this Edge's loader windows: the
	// process nodes that pass loaderCardNode's checks. A refusal at one of them
	// exists or not because this Edge says so.
	windows map[string]bool
	// processes are this Edge's process names. An ack whose AckProcessID is one
	// of them was answered here (AckSupplyRefusal records the process NAME).
	processes map[string]bool
}

// refusalOwnership reads this Edge's loader windows and process names. A
// failed read fails the apply: an empty window set would hand every refusal
// this Edge made to Core's say, and drop the ones Core never heard of.
//
// THE CLAIMS ARE READ HERE, NOT THROUGH claimAtNode. claimAtNode reads a
// failed claim lookup as "no claim", which is right for an operator door (the
// tap is refused and retried) and wrong here: one failed read would drop a
// window from the set and its refusal with it. NodeClaimsForStyles is the same
// lookup in one query that returns its error; the synthesized arm is the
// loader store's in-memory snapshot and cannot fail.
func (e *Engine) refusalOwnership() (refusalOwners, error) {
	procs, err := e.db.ListProcesses()
	if err != nil {
		return refusalOwners{}, fmt.Errorf("list processes: %w", err)
	}
	nodes, err := e.db.ListProcessNodes()
	if err != nil {
		return refusalOwners{}, fmt.Errorf("list process nodes: %w", err)
	}
	all := make([]*processes.Process, len(procs))
	byID := make(map[int64]*processes.Process, len(procs))
	for i := range procs {
		all[i] = &procs[i]
		byID[procs[i].ID] = &procs[i]
	}
	claims, err := e.db.NodeClaimsForStyles(store.ClaimStyleIDs(all...))
	if err != nil {
		return refusalOwners{}, fmt.Errorf("read node claims: %w", err)
	}
	own := refusalOwners{windows: map[string]bool{}, processes: map[string]bool{}}
	for _, p := range procs {
		if p.Name != "" {
			own.processes[p.Name] = true
		}
	}
	for i := range nodes {
		n := &nodes[i]
		claim := claims.Resolve(byID[n.ProcessID], n, store.ActiveStyleFirst)
		if claim == nil {
			claim = e.synthLoaderClaim(n.CoreNodeName)
		}
		if loaderCardShape(n) != nil || loaderCardClaim(n, claim) != nil {
			continue
		}
		own.windows[n.CoreNodeName] = true
	}
	return own, nil
}

type refusalKey struct{ loader, payload string }

// mergeRefusalSnapshot decides the open set this Edge keeps from its own rows
// (local) and Core's (core), and the messages that tell Core what it is
// missing. Per card:
//
//   - At one of this Edge's windows, existence is this Edge's: a row it has and
//     Core lacks is kept and Opened re-emitted; a row Core has and it lacks
//     stays absent and Closed is re-emitted.
//   - At any other window, existence is Core's: a row Core lacks is dropped,
//     with any ack on it (an ack on a refusal Core no longer holds is moot), and
//     a row Core has is taken from Core.
//   - Where both hold the row, every field comes from Core, except an ack this
//     Edge's process gave that Core does not have yet: that ack is kept and
//     Acked re-emitted. An ack Core already holds wins, including on a row at
//     this Edge's window, because Core keeps the first answer.
func mergeRefusalSnapshot(local []store.SupplyRefusal, core []protocol.SupplyRefusalState, own refusalOwners) ([]store.SupplyRefusal, []protocol.SupplyRefusalState) {
	atCore := make(map[refusalKey]protocol.SupplyRefusalState, len(core))
	for _, c := range core {
		if c.LoaderNode != "" && c.PayloadCode != "" {
			atCore[refusalKey{c.LoaderNode, c.PayloadCode}] = c
		}
	}
	var keep []store.SupplyRefusal
	var emits []protocol.SupplyRefusalState
	seen := make(map[refusalKey]bool, len(local)+len(core))
	for _, l := range local {
		k := refusalKey{l.LoaderNode, l.PayloadCode}
		seen[k] = true
		c, ok := atCore[k]
		switch {
		case ok:
			row, emit := mergeOpenRefusal(l, c, own)
			keep = append(keep, row)
			if emit != nil {
				emits = append(emits, *emit)
			}
		case own.windows[l.LoaderNode]:
			keep = append(keep, l)
			emits = append(emits, protocol.SupplyRefusalState{
				Action: protocol.SupplyRefusalOpened, LoaderNode: l.LoaderNode, PayloadCode: l.PayloadCode,
				RefusedAt: l.RefusedAt.UTC(), RefusedBy: l.RefusedBy,
			})
		}
	}
	for _, c := range core {
		k := refusalKey{c.LoaderNode, c.PayloadCode}
		if seen[k] || c.LoaderNode == "" || c.PayloadCode == "" {
			continue
		}
		seen[k] = true
		if own.windows[c.LoaderNode] {
			emits = append(emits, protocol.SupplyRefusalState{
				Action: protocol.SupplyRefusalClosed, LoaderNode: c.LoaderNode, PayloadCode: c.PayloadCode,
			})
			continue
		}
		keep = append(keep, refusalFromCore(c))
	}
	return keep, emits
}

// mergeOpenRefusal merges one card both sides hold open: Core's row, with this
// Edge's own ack kept (and re-emitted) where Core has none.
func mergeOpenRefusal(l store.SupplyRefusal, c protocol.SupplyRefusalState, own refusalOwners) (store.SupplyRefusal, *protocol.SupplyRefusalState) {
	row := refusalFromCore(c)
	if c.AckAt != nil || !l.Answered() || !own.processes[l.AckProcessID] {
		return row, nil
	}
	row.AckAt, row.AckChoice, row.AckProcessID = l.AckAt, l.AckChoice, l.AckProcessID
	at := l.AckAt.UTC()
	return row, &protocol.SupplyRefusalState{
		Action: protocol.SupplyRefusalAcked, LoaderNode: l.LoaderNode, PayloadCode: l.PayloadCode,
		AckAt: &at, AckChoice: l.AckChoice, AckProcessID: l.AckProcessID,
	}
}

func refusalFromCore(c protocol.SupplyRefusalState) store.SupplyRefusal {
	return store.SupplyRefusal{
		LoaderNode: c.LoaderNode, PayloadCode: c.PayloadCode,
		RefusedAt: c.RefusedAt, RefusedBy: c.RefusedBy,
		AckAt: c.AckAt, AckChoice: c.AckChoice, AckProcessID: c.AckProcessID,
	}
}

// refusalStates projects the open table onto the wire shape RefusalsDigest
// takes, the same shape Core digests its own rows in.
func refusalStates(open []store.SupplyRefusal) []protocol.SupplyRefusalState {
	out := make([]protocol.SupplyRefusalState, len(open))
	for i, r := range open {
		out[i] = protocol.SupplyRefusalState{
			Action: protocol.SupplyRefusalOpened, LoaderNode: r.LoaderNode, PayloadCode: r.PayloadCode,
			RefusedAt: r.RefusedAt, RefusedBy: r.RefusedBy,
			AckAt: r.AckAt, AckChoice: r.AckChoice, AckProcessID: r.AckProcessID,
		}
	}
	return out
}

// refusalsDigest is the FeedRefusals entry of the heartbeat: the digest of this
// Edge's open table, noted as held so the ack can confirm it. ok is false when
// the key is left out — the table could not be read (a digest of nothing would
// tell Core this Edge holds no refusals, and Core would answer with its set),
// or the last ack came from a Core that does not answer feeds, which would
// never compare it, so the read is skipped. A newer Core is therefore first
// asked on the heartbeat after its first ack.
func (e *Engine) refusalsDigest() (string, bool) {
	if !e.CoreSpeaksFeeds() {
		return "", false
	}
	open, err := e.db.ListOpenSupplyRefusals()
	if err != nil {
		e.logFn("feeds: read refusals for the heartbeat: %v — left out", err)
		return "", false
	}
	d, err := protocol.RefusalsDigest(refusalStates(open))
	if err != nil {
		e.logFn("feeds: digest refusals: %v — left out", err)
		return "", false
	}
	e.noteHeld(protocol.FeedRefusals, d)
	return d, true
}
