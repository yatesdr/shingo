package engine

import (
	"sort"
	"strings"
	"time"

	"shingo/protocol"
)

// owned.go — the Edge-owned direction of the digests: THE RULE of
// protocol/feeds.go run the other way. This Edge owns its plant claims, its
// bins watch list and its cell catalog, and Core holds a copy of each. The
// heartbeat quotes one digest per family (OwnedDigests); the ack answers each
// with Core's digest and, for a family that differs, the members Core holds
// (protocol.OwnedAnswer); the Edge sends the members that differ, under the
// same resend guard as everything it sends Core (sendDue), and converges the
// rest (answerOwned).
//
// A QUIET HEARTBEAT AND A QUIET ACK READ NOTHING AND HASH NOTHING. Each
// family's members and digest are cached here (ownedHeld). A family is built
// from the store at boot (Start), on the first heartbeat or ack that needs a
// family never built, by a save hook (the watch list's rebuild, a counter
// save; a publish of the claims hands over what it read, ClaimsReported), and
// on a floor of ownedFloor for a write no hook reaches. A digest is computed
// only in a build, a send or a save hook. PIN:
// TestOnCoreAck_AQuietAckRunsNoStatementAndNoHash,
// TestOwned_EveryOwnedDigestIsInABuildOrASend.
//
// AN OLDER CORE (an ack with no Owned) is answered by the same loop from the
// two fields it sends: Claims, every process Core holds a report for at its
// digest, and BinWatch, the digest of the watch list it holds. No cells are
// sent to it: the register carries them, as before. PIN:
// TestOnCoreAck_ClaimsReachThePublisherOnlyWhenSent,
// TestBinWatch_WhenTheListIsPublished.

// ownedFloor is how often every family is rebuilt from the store whatever its
// hooks said: the longest a write no hook reaches takes to reach Core.
const ownedFloor = 5 * time.Minute

// ownedBuild is a family as one build read it. members is member name to the
// member's digest (protocol.OwnedSingleton for a family of one). unread is the
// members whose own read failed: never sent and never emptied, since an
// unread spec is not evidence that a member is gone. raw is a family of one's
// value digest and value the value itself, sent from here.
type ownedBuild struct {
	members map[string]string
	unread  map[string]bool
	raw     string
	value   any
}

// ownedRow is everything the Edge says about one family of data it owns, in
// one place, as feedRow is for Core's feeds. TestOwnedRows_CoverAllOwned
// fails when a key of protocol.AllOwned has no row.
type ownedRow struct {
	key string
	// guard is the resend guard's key for one member, the text /status flags
	// it under ("claims:<process>", "bins.watch", "cells"); member reads it
	// back, ok false for a key that is not the family's.
	guard  func(member string) string
	member func(guardKey string) (string, bool)
	// build reads the family as this Edge holds it now. ok false (could not
	// read) leaves the family off the heartbeat: nothing sent, nothing
	// confirmed, until a build reads it.
	build func(e *Engine) (ownedBuild, bool)
	// send publishes one member to Core against quoted, Core's digest for it
	// ("" for none): its current value, or its empty value when this Edge no
	// longer holds it, under the resend guard. pending false: Core holds it
	// after all (a fresher read matched quoted), so it is converged.
	send func(e *Engine, held ownedHeld, member, quoted string) (pending bool)
	// legacy answers the family from an older Core's ack (no Owned): what Core
	// holds by member, what this Edge holds in the same terms, and whether the
	// ack carried the family at all. nil: an older Core does not serve it.
	legacy func(held ownedHeld, ack *protocol.EdgeHeartbeatAck) (core, mine map[string]string, served bool)
	// force sends the members on the first ack that answers the family even
	// where Core's digest is this Edge's (the watch list while no bins copy is
	// held: a restarted Edge asks for its snapshot by sending its list).
	force func(e *Engine) bool
	// check runs on every ack that answers the family, after what Core holds
	// is recorded: a look, from memory, at whether the family's inputs moved,
	// which rebuilds it and sends the move through its hook. The watch list's:
	// the stations' bin lists answer from memory until the plant moves
	// (binWatchChanged). nil: the family has no such look.
	check func(e *Engine)
}

// ownedRows has one row per Edge-owned family.
var ownedRows = [...]ownedRow{
	{key: protocol.OwnedClaims, guard: claimsGuardKey, member: claimsMemberOf,
		build: buildClaimsFamily, send: sendClaimsMember, legacy: legacyClaims},
	{key: protocol.OwnedBinWatch, guard: singletonGuard(binWatchKey), member: singletonMember(binWatchKey),
		build: buildBinWatchFamily, send: sendBinWatchMember, legacy: legacyBinWatch,
		force: func(e *Engine) bool { return e.nodeBins.Load() == nil },
		check: func(e *Engine) {
			if e.binWatchSource != nil {
				e.binWatchSource()
			}
		}},
	{key: protocol.OwnedCells, guard: singletonGuard(cellsKey), member: singletonMember(cellsKey),
		build: buildCellsFamily, send: sendCellsMember},
}

// ownedRowOf is the row for a family key.
func ownedRowOf(key string) (ownedRow, bool) {
	for _, r := range ownedRows {
		if r.key == key {
			return r, true
		}
	}
	return ownedRow{}, false
}

// ownedHeld is the cache of one family, under the feed book's lock.
type ownedHeld struct {
	// built: a build has been attempted; ok: the last one read the family.
	built, ok bool
	ownedBuild
	// digest is protocol.OwnedDigest of members; quoted is what the last
	// heartbeat named for the family.
	digest, quoted string
	// seq orders builds, so a slow build never replaces a newer one.
	seq uint64
	// served: the last ack answered the family; legacy: in an older Core's
	// fields; carried: some ack has since boot. coreHeld is what Core held by
	// member as of the last ack that said (an answer with members, or one
	// equal to the quote).
	served, legacy, carried bool
	coreHeld                map[string]string
	// builds counts the builds, so a test can tell a quiet ack built nothing
	// (TestOnCoreAck_AQuietAckRunsNoStatementAndNoHash). Written only by a
	// build, under the lock the build takes anyway.
	builds int
}

// mine is this Edge's members in the terms the last ack used.
func (h ownedHeld) mine(row ownedRow, ack *protocol.EdgeHeartbeatAck) map[string]string {
	if h.legacy && row.legacy != nil {
		_, mine, _ := row.legacy(h, ack)
		return mine
	}
	return h.members
}

func (b *feedBook) ownedLocked(key string) *ownedHeld {
	if b.owned == nil {
		b.owned = map[string]*ownedHeld{}
	}
	h := b.owned[key]
	if h == nil {
		h = &ownedHeld{}
		b.owned[key] = h
	}
	return h
}

// buildOwned reads one family from the store and holds it, with its digest.
// The one place a family is read and its digest computed.
func (e *Engine) buildOwned(row ownedRow) ownedHeld {
	e.feeds.mu.Lock()
	e.feeds.ownedSeq++
	seq := e.feeds.ownedSeq
	e.feeds.mu.Unlock()

	b, ok := row.build(e)
	var digest string
	if ok {
		d, err := protocol.OwnedDigest(b.members)
		if err != nil {
			e.logFn("owned: digest %s: %v — left off the heartbeat", row.key, err)
			ok = false
		}
		digest = d
	}

	e.feeds.mu.Lock()
	defer e.feeds.mu.Unlock()
	h := e.feeds.ownedLocked(row.key)
	h.builds++
	if seq < h.seq {
		return *h // a newer build already landed
	}
	h.seq, h.built, h.ok = seq, true, ok
	if ok {
		h.ownedBuild, h.digest = b, digest
	}
	return *h
}

// heldOwned is a family's cache, built first if it never was.
func (e *Engine) heldOwned(row ownedRow) ownedHeld {
	e.feeds.mu.Lock()
	h := *e.feeds.ownedLocked(row.key)
	e.feeds.mu.Unlock()
	if !h.built {
		return e.buildOwned(row)
	}
	return h
}

// buildAllOwned builds every family: at boot, and on the floor.
func (e *Engine) buildAllOwned() {
	e.feeds.mu.Lock()
	e.feeds.ownedFloorAt = e.feeds.clock()
	e.feeds.mu.Unlock()
	for _, row := range ownedRows {
		e.buildOwned(row)
	}
}

// OwnedDigests is the heartbeat's Owned map: per family this Edge could read,
// the digest of what it holds. Never nil, so Core can tell this Edge from one
// that predates the owned map. A quiet beat reads the cache only; the beat
// that reaches the floor rebuilds every family first.
func (e *Engine) OwnedDigests() map[string]string {
	e.feeds.mu.Lock()
	floor := e.feeds.clock().Sub(e.feeds.ownedFloorAt) >= ownedFloor
	e.feeds.mu.Unlock()
	if floor {
		e.buildAllOwned()
	}
	out := map[string]string{}
	for _, row := range ownedRows {
		if h := e.heldOwned(row); !h.ok {
			continue
		}
		e.feeds.mu.Lock()
		h := e.feeds.ownedLocked(row.key)
		h.quoted = h.digest
		out[row.key] = h.digest
		e.feeds.mu.Unlock()
	}
	return out
}

// answerOwned takes the ack's answer for every family: ack.Owned from a Core
// that speaks the owned map, else the older Core's two fields (ownedRow.legacy).
//
// For each family the ack answers: equal to the heartbeat's quote, the family
// has converged and every guard key of it settles (ClaimsSettled's rule, for
// every family); different, Core's members are diffed against this Edge's,
// each member that differs or that Core lacks is sent, each member Core holds
// that this Edge lacks is sent empty, and the rest settle. A family the ack
// does not answer sends and settles nothing. All of it from the cache: no
// statement and no digest unless something is sent.
func (e *Engine) answerOwned(ack *protocol.EdgeHeartbeatAck) {
	legacy := ack.Owned == nil
	for _, row := range ownedRows {
		var (
			core   map[string]string
			served bool
			equal  bool
		)
		if !legacy {
			var a protocol.OwnedAnswer
			a, served = ack.Owned[row.key]
			core = a.Members
			e.feeds.mu.Lock()
			equal = served && a.Digest == e.feeds.ownedLocked(row.key).quoted
			e.feeds.mu.Unlock()
		} else if row.legacy != nil {
			e.feeds.mu.Lock()
			snap := *e.feeds.ownedLocked(row.key)
			e.feeds.mu.Unlock()
			core, _, served = row.legacy(snap, ack)
		}
		e.feeds.mu.Lock()
		hp := e.feeds.ownedLocked(row.key)
		first := served && !hp.carried
		hp.served, hp.legacy = served, legacy
		hp.carried = hp.carried || served
		e.feeds.mu.Unlock()
		if !served {
			continue
		}
		h := e.heldOwned(row)
		if !h.ok {
			continue
		}
		if equal {
			e.setCoreHeld(row.key, h.members)
		} else {
			e.setCoreHeld(row.key, core)
		}
		if row.check != nil {
			// Against what Core has just said: a family moved since its last
			// build is rebuilt here, and its hook sends the move.
			row.check(e)
			h = e.heldOwned(row)
		}
		force := first && row.force != nil && row.force(e)
		if equal {
			if h.quoted != h.digest {
				continue // a build after the quote moved the family; its hook sent the change
			}
			if !force {
				e.settleOwned(row, nil)
				continue
			}
			core = h.members
		}
		mine := h.mine(row, ack)
		pending := map[string]bool{}
		for _, m := range memberNames(mine) {
			if h.unread[m] {
				pending[m] = true
				continue
			}
			q, has := core[m]
			if has && q == mine[m] && !force {
				continue
			}
			if row.send(e, h, m, q) {
				pending[m] = true
			}
		}
		for _, m := range memberNames(core) {
			if _, have := mine[m]; have || h.unread[m] {
				continue
			}
			if row.send(e, h, m, core[m]) {
				pending[m] = true
			}
		}
		e.settleOwned(row, pending)
	}
}

// ownedChanged is a family's save hook: rebuild it now and, while Core serves
// it, send each member that differs from what Core held at the last ack, so an
// edit reaches Core at once rather than at the next ack.
func (e *Engine) ownedChanged(key string) {
	row, ok := ownedRowOf(key)
	if !ok {
		return
	}
	h := e.buildOwned(row)
	if !h.ok || !h.served {
		return
	}
	mine := h.mine(row, nil)
	for _, m := range memberNames(mine) {
		switch q, has := h.coreHeld[m]; {
		case h.unread[m]:
		case has && q == mine[m]:
			e.converged(row.guard(m))
		default:
			row.send(e, h, m, q)
		}
	}
	for _, m := range memberNames(h.coreHeld) {
		if _, have := mine[m]; !have && !h.unread[m] {
			row.send(e, h, m, h.coreHeld[m])
		}
	}
}

// setCoreHeld records what Core holds of a family, by member.
func (e *Engine) setCoreHeld(key string, core map[string]string) {
	held := make(map[string]string, len(core))
	for m, d := range core {
		held[m] = d
	}
	e.feeds.mu.Lock()
	e.feeds.ownedLocked(key).coreHeld = held
	e.feeds.mu.Unlock()
}

// settleOwned converges every guard key of the family whose member is not in
// pending (nil: every one): Core holds what this Edge would send, or the
// member has left both sides. A member that leaves both sides is never quoted
// again, so this is the only place its streak and flag can end.
func (e *Engine) settleOwned(row ownedRow, pending map[string]bool) {
	e.feeds.mu.Lock()
	var settled []string
	for key := range e.feeds.sent {
		if m, ok := row.member(key); ok && !pending[m] {
			settled = append(settled, key)
		}
	}
	e.feeds.mu.Unlock()
	for _, key := range settled {
		e.converged(key)
	}
}

// updateOwnedMember replaces one member of a family as a send or a save just
// read it (present false: this Edge no longer holds it), and its digest.
func (e *Engine) updateOwnedMember(key, member, digest string, present bool) {
	e.feeds.mu.Lock()
	h := e.feeds.ownedLocked(key)
	if !h.built || !h.ok {
		e.feeds.mu.Unlock()
		return // the next build reads it
	}
	if d, had := h.members[member]; had == present && d == digest {
		e.feeds.mu.Unlock()
		return
	}
	members := make(map[string]string, len(h.members)+1)
	for m, d := range h.members {
		members[m] = d
	}
	if present {
		members[member] = digest
	} else {
		delete(members, member)
	}
	seq := h.seq
	e.feeds.mu.Unlock()
	d, err := protocol.OwnedDigest(members)
	if err != nil {
		e.logFn("owned: digest %s: %v", key, err)
		return
	}
	e.feeds.mu.Lock()
	defer e.feeds.mu.Unlock()
	h = e.feeds.ownedLocked(key)
	if h.seq != seq {
		return // a build landed meanwhile and read this member itself
	}
	h.members, h.digest = members, d
	if h.unread[member] {
		unread := make(map[string]bool, len(h.unread))
		for m := range h.unread {
			if m != member {
				unread[m] = true
			}
		}
		h.unread = unread
	}
}

// singletonGuard and singletonMember are a family of one's guard key: the one
// key, whatever the member.
func singletonGuard(key string) func(string) string { return func(string) string { return key } }

func singletonMember(key string) func(string) (string, bool) {
	return func(k string) (string, bool) { return protocol.SingletonMember, k == key }
}

// memberNames is m's keys in order, so sends go in a fixed order.
func memberNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ── plant claims ─────────────────────────────────────────────────────────────

// claimsKeyPrefix scopes the guard's keys for plant claims: "claims:<process>".
const claimsKeyPrefix = "claims:"

func claimsGuardKey(process string) string { return claimsKeyPrefix + process }

func claimsMemberOf(key string) (string, bool) { return strings.CutPrefix(key, claimsKeyPrefix) }

// ClaimsSource is the plant-claims publisher (messaging PlantClaimsPublisher)
// as the claims family reads and sends through it; the engine never imports
// the publisher.
type ClaimsSource interface {
	// ClaimsMembers reads this Edge's spec: every process with styles, name to
	// its report's digest; unread, the processes whose own read failed; err,
	// the process list could not be read.
	ClaimsMembers() (members map[string]string, unread map[string]bool, err error)
	// SendClaims reads one process's report (the empty report when this Edge
	// has no process of that name, or it has no styles) and publishes it when
	// due says so; digest and styled describe what was read, sent or not.
	SendClaims(name string, due func(digest string) bool) (digest string, styled bool, err error)
}

// SetClaimsSource wires the claims family to the publisher. Set before Start,
// so the boot build reads the claims. Setting it again rebuilds on next use.
func (e *Engine) SetClaimsSource(src ClaimsSource) {
	e.feeds.mu.Lock()
	e.claimsSource = src
	e.feeds.ownedLocked(protocol.OwnedClaims).built = false
	e.feeds.mu.Unlock()
}

func (e *Engine) claimsSourceNow() ClaimsSource {
	e.feeds.mu.Lock()
	defer e.feeds.mu.Unlock()
	return e.claimsSource
}

// ClaimsReported is the publisher's save hook: it built one process's report
// (an edit, a style flip) and publishes it. The claims family takes the
// digest, so the next heartbeat quotes it, and the publish is recorded as a
// push, so a Core quoting it while a later save's is on its way is converging
// (pinned by TestOwned_ASavesPublishQuotedBackConverges). styled false: the
// process has no styles, or no longer exists, and is no member.
func (e *Engine) ClaimsReported(process, digest string, styled bool) {
	e.recordPush(claimsGuardKey(process), digest)
	e.updateOwnedMember(protocol.OwnedClaims, process, digest, styled)
}

// ClaimsReportedAll is the publisher's full read (boot, a register, the
// hourly snapshot for an older Core): it replaces the claims family.
func (e *Engine) ClaimsReportedAll(members map[string]string, unread map[string]bool) {
	d, err := protocol.OwnedDigest(members)
	if err != nil {
		e.logFn("owned: digest %s: %v", protocol.OwnedClaims, err)
		return
	}
	e.feeds.mu.Lock()
	defer e.feeds.mu.Unlock()
	h := e.feeds.ownedLocked(protocol.OwnedClaims)
	e.feeds.ownedSeq++
	h.seq, h.built, h.ok = e.feeds.ownedSeq, true, true
	h.ownedBuild = ownedBuild{members: members, unread: unread}
	h.digest = d
}

func buildClaimsFamily(e *Engine) (ownedBuild, bool) {
	src := e.claimsSourceNow()
	if src == nil {
		return ownedBuild{}, false
	}
	members, unread, err := src.ClaimsMembers()
	if err != nil {
		e.logFn("owned: read the plant claims: %v — nothing published", err)
		return ownedBuild{}, false
	}
	return ownedBuild{members: members, unread: unread}, true
}

// sendClaimsMember re-reads the process and publishes its report unless Core
// already holds that digest; the family takes what was read.
func sendClaimsMember(e *Engine, _ ownedHeld, process, quoted string) bool {
	src := e.claimsSourceNow()
	if src == nil {
		return true
	}
	key := claimsGuardKey(process)
	held := true
	digest, styled, err := src.SendClaims(process, func(d string) bool {
		if d == quoted {
			held = false
			return false
		}
		return e.sendDue(key, d, quoted)
	})
	if err != nil {
		e.logFn("plant_claims: reconcile with Core: %s: %v — left as it is", process, err)
		return true
	}
	e.updateOwnedMember(protocol.OwnedClaims, process, digest, styled)
	return held
}

// legacyClaims is an older Core's Claims map: every process it holds, at its
// digest, on every ack; nil is a Core that does not quote claims.
func legacyClaims(h ownedHeld, ack *protocol.EdgeHeartbeatAck) (core, mine map[string]string, served bool) {
	if ack == nil {
		return nil, h.members, false
	}
	return ack.Claims, h.members, ack.Claims != nil
}

// ── the cell catalog ─────────────────────────────────────────────────────────

// cellsKey is the resend guard's key for the cell catalog.
const cellsKey = "cells"

// SetCellCatalogFunc wires the cells family's read: this Edge's reporting
// points grouped by PLC (messaging BuildCellCatalog over the store's counted
// points, at the composition root). nil leaves the family off the heartbeat.
func (e *Engine) SetCellCatalogFunc(fn func() ([]protocol.CellCatalogEntry, error)) {
	e.feeds.mu.Lock()
	e.cellCatalogFn = fn
	e.feeds.ownedLocked(protocol.OwnedCells).built = false
	e.feeds.mu.Unlock()
}

// CellsChanged is the cells family's save hook: a process's counter or its
// reporting point was saved (EditProcessCounter). The catalog is read again
// and, while Core serves the family, sent at once when it moved.
func (e *Engine) CellsChanged() { e.ownedChanged(protocol.OwnedCells) }

// buildCellsFamily reads the catalog: one member at CellCatalogDigest of it,
// none for a catalog of no cells, the catalog kept to send.
func buildCellsFamily(e *Engine) (ownedBuild, bool) {
	e.feeds.mu.Lock()
	fn := e.cellCatalogFn
	e.feeds.mu.Unlock()
	if fn == nil {
		return ownedBuild{}, false
	}
	cells, err := fn()
	if err != nil {
		e.logFn("owned: read the cell catalog: %v — left off the heartbeat", err)
		return ownedBuild{}, false
	}
	if cells == nil {
		cells = []protocol.CellCatalogEntry{}
	}
	digest, err := protocol.CellCatalogDigest(cells)
	if err != nil {
		e.logFn("owned: digest the cell catalog: %v — left off the heartbeat", err)
		return ownedBuild{}, false
	}
	return ownedBuild{members: protocol.OwnedSingleton(digest, len(cells) == 0), raw: digest, value: cells}, true
}

// sendCellsMember sends the catalog the family holds (the empty catalog when
// this Edge has no cell: Core empties the station's cells on it) as
// edge.cells, through the outbox, where a newer catalog supersedes an unsent
// older one (store/messaging coalescableSubjects). Never as a register: its
// handling re-runs the claims publish and the loader sweeps.
func sendCellsMember(e *Engine, h ownedHeld, _, quoted string) bool {
	if e.db == nil || !e.sendDue(cellsKey, h.raw, quoted) {
		return true
	}
	cells, _ := h.value.([]protocol.CellCatalogEntry)
	if cells == nil {
		cells = []protocol.CellCatalogEntry{}
	}
	env, err := protocol.NewDataEnvelope(
		protocol.SubjectEdgeCells,
		protocol.Address{Role: protocol.RoleEdge, Station: e.cfg.StationID()},
		protocol.Address{Role: protocol.RoleCore},
		&protocol.CellCatalog{Digest: h.raw, Cells: cells},
	)
	if err != nil {
		e.logFn("owned: build edge.cells: %v", err)
		return true
	}
	data, err := env.Encode()
	if err != nil {
		e.logFn("owned: encode edge.cells: %v", err)
		return true
	}
	if err := e.db.EnqueueSnapshotOutbox([][]byte{data}, protocol.SubjectEdgeCells); err != nil {
		e.logFn("owned: enqueue edge.cells (%d cells): %v — the next ack sends it again", len(cells), err)
	}
	return true
}
