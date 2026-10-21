// Package resend is the resend guard on a digest compare: the rule the side
// that owns some data applies before sending it again to a side that quotes a
// different digest. Core applies it to its feeds (shingo-core messaging
// feeds.go compare), the Edge to what it owns and sends to Core, its plant
// claims and its bins watch list (shingo-edge engine feeds.go sendDue).
//
// It is shared because the two copies drifted: the rule that a receiver
// quoting the last send has converged (so a value that moves on every
// heartbeat is not "stuck") was found on Core and mirrored to the Edge by
// hand, and the Edge's copy read the wall clock where Core's read its book's.
// Each caller keeps what is its own: where the records live, the flag text,
// the log line, and on Core the push record beside the compare's.
package resend

import "time"

const (
	// Guard: a send of the same digest of the same key this recently is held
	// back. The copy is in flight, and the next heartbeat may simply predate
	// its arrival. It also bounds a key that never converges at one send per
	// Guard.
	Guard = 2 * time.Minute
	// FlagAfter is how many sends in a row, with the receiver never quoting
	// the last one back, mark a key as not converging.
	FlagAfter = 3
)

// Record is the last send of one key to one receiver. The zero value is a
// key never sent.
type Record struct {
	digest string
	at     time.Time
	streak int
	// held: the receiver has quoted a send back since the last one.
	held bool
}

// IsLast reports whether d is the digest last sent: a receiver quoting it
// holds that send, so the key has converged even if the value has moved on.
func (r *Record) IsLast(d string) bool { return d == r.digest }

// Converged records that the receiver holds what it was sent: the streak ends,
// and a later quote of anything else is the receiver having lost it.
func (r *Record) Converged() { r.streak, r.held = 0, true }

// Send reports whether digest is due at now and, when it is, records the send
// and counts it in the streak. quoted is the digest the receiver holds for the
// key ("" for none, or not asked). The same digest inside Guard is held back
// and changes nothing: that copy is in flight. Not so when the receiver held
// the last send and now quotes something else: it lost it (it restarted), so
// nothing is in flight and the same digest goes at once. A restarted receiver
// is healed by its first heartbeat, not two minutes later.
func (r *Record) Send(digest, quoted string, now time.Time) bool {
	forgotten := r.held && quoted != r.digest
	if r.digest == digest && !forgotten && now.Sub(r.at) < Guard {
		return false
	}
	r.digest, r.at, r.held = digest, now, false
	r.streak++
	return true
}

// Streak is the sends in a row since the key last converged.
func (r *Record) Streak() int { return r.streak }

// NotConverging reports whether the streak has reached FlagAfter.
func (r *Record) NotConverging() bool { return r.streak >= FlagAfter }

// Last is the digest last sent and when; "" and zero before any send.
func (r *Record) Last() (digest string, at time.Time) { return r.digest, r.at }
