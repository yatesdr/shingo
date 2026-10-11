package messaging

import (
	"database/sql"
	"fmt"

	"shingo/protocol"
)

// Execer is the minimal write surface an outbox insert needs, satisfied by
// both *sql.DB and *sql.Tx. Same idiom as audit.BinUOPExecer, and here for
// the same reason: a message that must commit with the work that caused it
// has to be written on that work's transaction.
type Execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// EpochAnnounce is the addressing needed to tell the plant that a carrier's
// generation has changed. It lives here, in the outbox package, because both
// senders need it and they sit either side of an import boundary that exists
// on purpose: the manifest service announces a reset, the delta applier answers
// a discarded count, and the applier must not import the service.
//
// Passed at construction rather than set afterwards, so a call site cannot end
// up with something that changes generations and tells nobody — which is the
// state both plants were in.
//
// Point-to-point: every sender addresses the station behind the carrier. The
// lifecycle announcements (a reset's bump, a count) were broadcast, and every
// Edge that does not model the node parsed each one and logged "process node
// not found"; they now go by the landing's address rule
// (service.announceLifecycle), so there is no broadcast sender left.
type EpochAnnounce struct {
	// Topic is the dispatch topic the outbox row is written to. Empty means
	// unwired; senders log rather than swallow.
	Topic string
	// CoreStation is this Core's station id — the envelope's sender.
	CoreStation string
}

// Wired reports whether this announcer can send.
func (a EpochAnnounce) Wired() bool { return a.Topic != "" }

// SendTo enqueues a Core→Edge message addressed to one station on ex — a
// *sql.Tx to make it commit with the work that caused it. The landing (Lane H,
// H.3), the refresh (§9.4) and the lifecycle announcements (Lane 0 item 5) know
// which station is behind the carrier, so they say so instead of broadcasting:
// the other stations would only parse it and drop it.
func (a EpochAnnounce) SendTo(ex Execer, subject, station string, payload any) error {
	return EnqueueDataToEdge(ex, a.Topic, subject, a.CoreStation, station, payload)
}

// BuildDataEnvelope builds a Core→Edge data-channel envelope and returns its
// encoded bytes plus the outbox msg_type. Split out so the two enqueue paths
// — the engine's plain send and the in-transaction send below — cannot drift
// in how they address a message.
func BuildDataEnvelope(subject, coreStation, edgeStation string, payload any) ([]byte, string, error) {
	coreAddr := protocol.Address{Role: protocol.RoleCore, Station: coreStation}
	edgeAddr := protocol.Address{Role: protocol.RoleEdge, Station: edgeStation}
	env, err := protocol.NewDataEnvelope(subject, coreAddr, edgeAddr, payload)
	if err != nil {
		return nil, "", fmt.Errorf("build data %s: %w", subject, err)
	}
	data, err := env.Encode()
	if err != nil {
		return nil, "", fmt.Errorf("encode data %s: %w", subject, err)
	}
	return data, "data." + subject, nil
}

// EnqueueDataToEdge builds the envelope and writes the outbox row on ex.
//
// Pass a *sql.Tx and the message becomes part of that transaction: it is
// delivered if and only if the work that caused it commits. That is the whole
// point of writing a row rather than sending a notification — an outbox row is
// not a message, it is a record that a message is owed, and the drainer turns
// it into one after the commit by construction. Announcing after commit
// instead leaves a window where the commit lands and the process dies before
// the send, which in this system means a station that is quietly wrong with
// nothing anywhere recording that it is.
func EnqueueDataToEdge(ex Execer, topic, subject, coreStation, edgeStation string, payload any) error {
	data, msgType, err := BuildDataEnvelope(subject, coreStation, edgeStation, payload)
	if err != nil {
		return err
	}
	if err := EnqueueOutbox(ex, topic, data, msgType, edgeStation); err != nil {
		return fmt.Errorf("enqueue data %s: %w", subject, err)
	}
	return nil
}
