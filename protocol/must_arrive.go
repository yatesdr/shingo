package protocol

import "time"

// must_arrive.go — the one table of messages that must not be lost.
//
// A message belongs here when a lost copy costs more than a late one, and a
// late one is safe by its receiver's guard. That is a property of the
// receiving handler, not of the sender, so a row is added only after checking
// what that handler does with a copy that arrives after newer ones (its dedup
// key and ordering guard), and the row says what the guard is.
//
// Two mechanisms keep a message, and they are different facts, so each row
// says which it takes:
//
//   - NeverExpires: its envelope carries no exp (NoExpiry), so no ingestor
//     drops it for arriving late (DataTTLFor, DefaultTTLFor);
//   - NeverDeadLetters: a failed publish spends no retry, so the outbox offers
//     it on every pass until the broker takes it (outbox.Drainer).
//
// Both lists are derived from this table and spelled nowhere else: the
// never-expire set by name, the never-dead-letter set by the key the sending
// side's outbox row carries (OutboxKey). TestNoExpiry_ExactMembership and
// TestNeverDeadLetter_ExactMembership pin the two sets, and docs/wire-protocol.md
// renders the table (MustArriveTable).

// Sender is the side whose outbox writes a message's row; for a data subject
// it decides the row's key.
type Sender int

const (
	// SentByEdge: the Edge writes a data subject's row under the bare subject.
	SentByEdge Sender = iota + 1
	// SentByCore: Core writes a data subject's row as "data."+subject
	// (shingo-core/store/messaging).
	SentByCore
	// SentByCoreReply: Core writes a reply's row as "data.reply."+subject
	// (shingo-core/messaging.CoreHandler).
	SentByCoreReply
)

// MustArrive is one message that must not be lost.
type MustArrive struct {
	// Name is the data subject, or the envelope type when Type is set.
	Name string
	// Type marks an envelope type rather than a data subject. An envelope
	// type's outbox row is keyed by the bare type on either side.
	Type bool
	// Sender is the side that writes its outbox row.
	Sender Sender
	// NeverExpires: the envelope carries no exp.
	NeverExpires bool
	// NeverDeadLetters: a failed publish spends no retry.
	NeverDeadLetters bool
	// Lost is what a lost copy costs.
	Lost string
	// Late is what makes a late copy safe at its receiver.
	Late string
}

// OutboxKey is the msg_type the sending side's outbox row carries: an envelope
// type and an Edge's data subject bare, a Core data subject as "data."+subject
// and a Core reply as "data.reply."+subject.
func (m MustArrive) OutboxKey() string {
	switch {
	case m.Type, m.Sender == SentByEdge:
		return m.Name
	case m.Sender == SentByCoreReply:
		return "data.reply." + m.Name
	default:
		return "data." + m.Name
	}
}

var mustArrive = []MustArrive{
	// The sequenced inventory counts carry information nothing else
	// resupplies. Most other data subjects are snapshots whose successor
	// carries the same truth a few seconds later; these are increments, and a
	// dropped one is a permanently wrong count that never self-corrects.
	//
	// A late copy is not always harmless. Core dedups on a per-scope high-water
	// SequenceID, so a replay is a no-op, but a delta that arrives after a
	// higher seq of its scope is skipped with no ledger row; and a delta from a
	// superseded epoch goes to the discrepancy audit, not the count. Expiring
	// them would lose more: a dropped delta is lost with no record at all.
	//
	// Measured at Springfield 2026-08-21, before the edge was hardwired: ~17
	// bin_uop_delta a day arrived past the 5-minute default and were discarded
	// by the ingestor before any handler ran, averaging 142 minutes late and
	// peaking at 23 hours. The edge marked every one of them sent.
	{
		Name: SubjectBinUOPDelta, Sender: SentByEdge, NeverExpires: true, NeverDeadLetters: true,
		Lost: "parts counted that Core never hears of: a permanently wrong count, with no record of it",
		Late: "Core dedups on the scope's high-water sequence; a delta from an ended generation goes to the discrepancy audit, not the count",
	},
	// The lineside bucket level is a snapshot, but of one row, and its successor
	// comes only when the pile next changes, which for a stranded row is never.
	{
		Name: SubjectLinesideBucketLevel, Sender: SentByEdge, NeverExpires: true, NeverDeadLetters: true,
		Lost: "Core's mirror of the pile is wrong until the pile next changes, which for a stranded pile is never",
		Late: "Core applies a level only above the row's high-water sequence",
	},
	// Shipped direct by a cursor over counter_snapshots, not through the
	// outbox, so it has no outbox row to exempt.
	{
		Name: SubjectProductionTicks, Sender: SentByEdge, NeverExpires: true,
		Lost: "a fake stop in the cell's MTBF and lost time",
		Late: "Core keys each tick on (cell, edge snapshot id, recorded at), so a late or repeated copy is a no-op",
	},
	// A dropped hold's row is the only record that those parts left the count.
	{
		Name: SubjectHoldDropped, Sender: SentByEdge, NeverExpires: true, NeverDeadLetters: true,
		Lost: "parts that left the count with no row anywhere saying so",
		Late: "Core keys the row on the envelope id, so a late copy writes it late and a redelivered one writes nothing",
	},
	// Core's count announcements to the Edge. One dropped after an outage
	// longer than TypeData's 5 minutes leaves the station holding a carrier
	// under a generation that has ended, so every count it reports is
	// discarded at Core. A late copy cannot walk a carrier's epoch backward:
	// every Edge write they reach goes through the store's epochAssignOnBind,
	// which moves a bound carrier's stamp forward only, and a bind into an
	// empty slot is refused for the carrier that last left it at an older
	// stamp (BindEmptySlotUnlessDeparted). A late UOPAdjustment's count is
	// safe when it carries a fence, which every count this Core states does,
	// per addressed station: the station rebases it on its own stream
	// (shingo-edge's Engine.fencedCount). Only an unfenced copy, an older
	// Core's, writes its count as sent over the ticks counted since.
	{
		Name: SubjectUOPAdjustment, Sender: SentByCore, NeverExpires: true, NeverDeadLetters: true,
		Lost: "a reset's number never reaches the station, and a person's count is never recovered on it",
		Late: "a count Core states is fenced per addressed station and rebased on its stream; a Released clears only a seat still bound to its bin by a landing the move ended (PlacedSeq)",
	},
	{
		Name: SubjectBinEpochRefresh, Sender: SentByCore, NeverExpires: true, NeverDeadLetters: true,
		Lost: "the station keeps counting under a generation that has ended, and Core discards every count it reports",
		Late: "a bound carrier's stamp only moves forward (epochAssignOnBind)",
	},
	// The lift of a bin (R4-7). Dropped, it strands what only it triggers on
	// the Edge: the slot's flush and clear, a changeover supply deferred to
	// this lift, the drop's line-cleared advance, the departure stamp, the
	// auto-push re-pull and a consolidation's Order B. Each branch was audited
	// for a late copy (HandleBinPickedUp): the stamp, the flush and the
	// line-cleared advance are idempotent; the deferred supply is released
	// only if it was never released past a wait and Core still takes it; the
	// re-pull asks Core's occupancy first; Order B's intent is taken once. The
	// clear is gated on the bin still bound at the slot, so the one shape a
	// late copy can still hurt is the SAME bin brought back to the SAME slot
	// (a carryover round trip) before the copy lands — a window the 5-minute
	// TTL already allowed for five minutes.
	{
		Name: SubjectBinPickedUp, Sender: SentByCore, NeverExpires: true, NeverDeadLetters: true,
		Lost: "strands what only the lift triggers: the slot's flush and clear, a changeover supply deferred to it, the re-pull",
		Late: "each branch is idempotent or gated on the bin still bound at the slot (HandleBinPickedUp)",
	},
	// The drop of a bin (Lane H, H.6): the only door that binds a carrier Core
	// placed. The seat keeps the placement stamp of the landing it last bound
	// (placed_seq, DECISIONS D10), and the bind's WHERE refuses one not newer,
	// so a redelivered or overtaken landing binds nothing; the same bin at an
	// equal or newer generation keeps its count (H.4).
	{
		Name: SubjectBinLanded, Sender: SentByCore, NeverExpires: true, NeverDeadLetters: true,
		Lost: "a seat left empty while a carrier stands on it, and every tick there held",
		Late: "the bind refuses a landing not newer than the seat's placed_seq",
	},
	// The ask pair (ask.go): the ask keeps AskTTL; the answer never expires.
	{
		Name: SubjectEdgeAnswer, Sender: SentByCoreReply, NeverExpires: true, NeverDeadLetters: true,
		Lost: "the asking Edge's table and seat stay wrong until something else heals them",
		Late: "an answer is a fact already true at Core",
	},
	// The demand episodes the Edge owns, as whole state (SubjectDemandOrigin).
	// Sent once per change on the outbox; the Edge re-sends one only when the
	// outbox refused it, not one sent and lost, so a dead-lettered or expired
	// copy was missing at Core until the episode next changed.
	{
		Name: SubjectDemandOrigin, Sender: SentByEdge, NeverExpires: true, NeverDeadLetters: true,
		Lost: "an episode's open or close is missing at Core until the episode next changes; a zero-order episode may never show",
		Late: "Core upserts on origin_id under a strictly rising revision (UpsertDemandOrigin), so a late copy loses the comparison",
	},
	// An ingest is a produced bin's count.
	{
		Name: TypeOrderIngest, Type: true, Sender: SentByEdge, NeverExpires: true, NeverDeadLetters: true,
		Lost: "a full produce bin left unconfirmed and invisible to sourcing",
		Late: "Core fences a late ingest by its bin_epoch",
	},
	// A release (R4-8). It fell to the 10-minute fallback, and one dropped
	// after a longer outage left the Edge's leg in_transit (the Edge marks it
	// so itself) and Core's staged: a robot nobody can release. A late copy
	// releases the wait it was pressed for, because the robot cannot pass a
	// station wait without it.
	{
		Name: TypeOrderRelease, Type: true, Sender: SentByEdge, NeverExpires: true, NeverDeadLetters: true,
		Lost: "a robot Core holds staged that nobody can release",
		Late: "the release echoes the wait it was pressed for (OrderRelease.StationWait), so a copy for a wait already passed is a no-op",
	},
	// A cancel. It fell to 10 minutes, and one dropped after a longer outage
	// left the Edge's row cancelled (the Edge ends it itself,
	// orders.Manager.AbortOrderWithReason) and Core's live: robots parked that
	// nothing on the Edge ever asks about again, since its status sync lists
	// only live rows. The Edge stamps the exp at enqueue and Core only reads
	// it, so this is the Edge's release: an older Core reads no exp as never
	// expiring (IsExpiredHeader).
	{
		Name: TypeOrderCancel, Type: true, Sender: SentByEdge, NeverExpires: true, NeverDeadLetters: true,
		Lost: "a robot keeps working an order its operator ended",
		Late: "one that does not ask cancels what is still live, and one that asks (OrderCancel.Intent) only an order still on its way; either writes nothing at Core on an order that has ended (dispatch.HandleOrderCancel)",
	},
	// A confirm (final brief 5i). It expired at 30 minutes, so one lost in a
	// longer outage left Core reading delivered until its sweep confirmed the
	// order, and for good on an order the sweep skips (skip_auto_confirm). The
	// Edge stamps the exp at enqueue and its own outbox spends the retries, so
	// this is the Edge's release, as the cancel's was.
	{
		Name: TypeOrderReceipt, Type: true, Sender: SentByEdge, NeverExpires: true, NeverDeadLetters: true,
		Lost: "a cart confirmed at the station that Core reads delivered until its sweep confirms it, and for good on an order the sweep skips",
		Late: "Core confirms only a delivered order, and a confirmed one is a no-op (dispatch.HandleOrderReceipt, LifecycleService.ConfirmReceipt)",
	},
	// Core's status tell (Core writes the row as "order.update"). It keeps its
	// 10-minute TTL (defaultTTLs): only the dead letter is lifted.
	{
		Name: TypeOrderUpdate, Type: true, Sender: SentByCore, NeverDeadLetters: true,
		Lost: "the station shows a status Core has left, and nothing repairs it on a healthy link",
		Late: "it carries the order's version, and a station applies only a newer one",
	},
}

// MustArriveTable is a copy of the table, in its order.
func MustArriveTable() []MustArrive {
	return append([]MustArrive(nil), mustArrive...)
}

// NeverDeadLetterKeys is the outbox key (OutboxKey) of every message that
// never dead-letters: the set outbox.Drainer exempts from MaxRetries.
func NeverDeadLetterKeys() map[string]bool {
	out := map[string]bool{}
	for _, m := range mustArrive {
		if m.NeverDeadLetters {
			out[m.OutboxKey()] = true
		}
	}
	return out
}

// withNeverExpires is ttls with NoExpiry added for every row that never
// expires and is a type (types) or a data subject (!types). A row the map
// also times is a contradiction between the two files, so it panics, which
// every test of the package meets at init.
func withNeverExpires(ttls map[string]time.Duration, types bool) map[string]time.Duration {
	for _, m := range mustArrive {
		if !m.NeverExpires || m.Type != types {
			continue
		}
		if _, timed := ttls[m.Name]; timed {
			panic("protocol: " + m.Name + " never expires in must_arrive.go and is also timed in expiry.go")
		}
		ttls[m.Name] = NoExpiry
	}
	return ttls
}
