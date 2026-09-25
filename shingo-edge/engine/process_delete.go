package engine

import (
	"database/sql"
	"errors"
	"fmt"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// process_delete.go — deleting a process ENDS its demands, out loud.
//
// ── WHAT THIS REPLACES ────────────────────────────────────────────────────
//
// processes.Delete drops the process's rows in demand_origins_open inside its
// own transaction and sends Core nothing. Edge forgets the episodes; Core does
// not, because nobody told it. Its rows stay open with no Edge row left that
// could ever close them, and the only thing that eventually notices is Core's
// childless pass, which closes them `unattributed` — a reason that means "this
// episode never had an order" and says nothing whatever about a process being
// deleted. Every process delete therefore manufactured a small pile of episodes
// that look, on every surface keyed on the demand grain, like demands the plant
// failed to serve.
//
// ── WHY THE VERB IS HERE AND NOT ON ProcessService ────────────────────────
//
// The close writer is Engine.closeEpisode, and it has to be the one used: it
// enqueues the closing STATE to the durable outbox and only then deletes the
// open row, which is the ordering that makes a close impossible to lose. A
// second writer spelled out in the store or the service layer would be a second
// message shape to keep in step, which is the drift emitOriginState exists to
// prevent.
//
// engine imports service, so service cannot call back into the engine; and www
// may not import the store at all (the `www-no-direct-store` depguard rule). So
// the composition — refuse, close, delete — lives on the engine, www calls this
// one verb, and the close stays in exactly one place. It sits beside
// SyncProcessCounter on EngineOrchestration for the same reason that one does:
// a process-lifecycle action that spans more than the process aggregate.
//
// ── THE ORDER IS THE DESIGN, NOT AN ARRANGEMENT ───────────────────────────
//
// Close, then delete. Walking the three windows a crash can land in:
//
//   - after a close is enqueued, before its open row is deleted. The outbox has
//     the close and the row is still on disk, so the episode reads as open until
//     the reconciling sweep closes it again at a higher revision — a no-op under
//     Core's revision guard. Nothing is lost. This window is closeEpisode's own
//     and predates this file.
//   - after the episodes are closed, before the process row is deleted. Core has
//     every close; Edge has no open row; the process still exists. The plant may
//     open a fresh episode for it on the next tick, which is honest — the
//     process is still there and still below its level — and the operator's
//     retry closes that one too. Nothing is stranded.
//   - after the process row is deleted, before the close was enqueued. THIS ONE
//     CANNOT HAPPEN. The delete is reached only from below, after every close
//     has returned, and a close that failed to enqueue refuses the delete
//     outright. There is no path that removes the process first.
//   - after the process row is deleted, before the pile levels are enqueued.
//     The piles are gone on the Edge and Core's mirror keeps them: the boot
//     resend sends only rows that exist. The window is the in-memory mark and
//     one flush after the commit; nothing on the Edge closes it afterwards.
//
// The process's lineside piles go with it (processes.Delete deletes them in its
// transaction). Their keys are read first, and each level is sent as 0 after
// the delete, so Core's mirror loses them too.
//
// ── THE SINGLE CONNECTION ─────────────────────────────────────────────────
//
// store.Open pins the edge to ONE SQLite connection, and processes.Delete holds
// it for its transaction. Every statement in this file is issued before that
// transaction begins — the name, the lists, and all of the closes —
// so nothing here ever calls *sql.DB while the transaction holds the connection.
// That is not tidiness: a close issued from inside the transaction would wait on
// a connection the same goroutine is holding, and wait forever. It is the same
// hazard the DBTX note in store/processes/processes.go describes, which is why
// the close is composed above the delete rather than handed to it as a callback.

// DeleteProcess deletes a process and closes the demand episodes it owns.
//
// The close is `claim_removed`: the need did not recover, it stopped being
// asked — which is exactly what deleting the process that was asking does. It is
// `notification` rather than `sweep` because something told us; the operator's
// delete is the event.
func (e *Engine) DeleteProcess(id int64) error {
	// The NAME, read here rather than through processName, because the three
	// answers are three different dispositions and processName collapses two of
	// them into "". A missing row is a double-click and is not an error — the
	// store's own delete says the same. A read that FAILED is not permission to
	// carry on: without the name the episodes cannot be found, and deleting the
	// process anyway is the silent stranding this verb exists to end.
	proc, err := processes.Get(e.db.DB, id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("delete process %d: read name: %w", id, err)
	}

	if proc.Name == "" {
		// An episode key built on an empty process names no place, and a list
		// keyed on "" would match every other episode whose process could not be
		// resolved. Closing those would end demands belonging to processes that
		// are still running, so this closes nothing and says so. The delete
		// proceeds: refusing it would leave a process nobody can remove.
		e.logFn("demand_episode: process %d has an empty name — deleting it without closing any "+
			"episode. A list keyed on an empty process would match other processes' unresolved "+
			"episodes, and closing those would end demands that are still being asked.", id)
	} else if err := e.closeProcessEpisodes(proc.Name); err != nil {
		return err
	}

	// The piles the delete takes, read while they still exist.
	piles, err := e.db.ListLinesidePileKeysForProcess(id)
	if err != nil {
		return fmt.Errorf("delete process %d: list lineside piles: %w", id, err)
	}
	e.countMu.Lock()
	defer e.countMu.Unlock()
	if err := e.processService.Delete(id); err != nil {
		return err
	}
	if len(piles) > 0 && e.inventoryDelta != nil {
		e.inventoryDelta.PilesChanged(piles...)
	}
	return nil
}

// closeProcessEpisodes closes every episode open for a process name, through the
// ordinary close writer.
//
// ONE LIST, ONE CLOSE PER OPEN EPISODE, AT DELETE ONLY. No tick, no sweep, no
// periodic work: a process is deleted by hand, perhaps twice a year, and this
// runs then and only then.
//
// It stops at the first failure and the delete is refused with it. Episodes
// closed before that one stay closed — legitimately, their closes are on the
// outbox — and the process is still there, which leaves the reconciling sweep
// holding a picture it understands and the operator a retry that finishes the
// job. The alternative is to carry on and delete the process anyway, which would
// destroy the open row of an episode whose close never reached the outbox: the
// one way this lane could lose a close outright.
func (e *Engine) closeProcessEpisodes(name string) error {
	open, err := e.db.ListOpenDemandOriginsForProcess(name)
	if err != nil {
		return fmt.Errorf("delete process %q: list open episodes: %w", name, err)
	}
	for i := range open {
		ep := &open[i]
		e.logFn("demand_episode: process %q is being deleted — closing origin=%s key=%s kind=%s",
			name, ep.OriginID, ep.EpisodeKey, ep.Kind)
		if err := e.closeEpisode(ep.EpisodeKey, protocol.CloseReasonClaimRemoved, protocol.ClosedByNotification); err != nil {
			return fmt.Errorf("delete process %q: %w", name, err)
		}
	}
	return nil
}
