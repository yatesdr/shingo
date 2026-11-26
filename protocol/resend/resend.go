// Package resend is the resend guard on a digest compare: the rule the side
// that owns some data applies before sending it again to a side that quotes a
// different digest. Core applies it to its feeds (shingo-core messaging
// feeds.go compare), the Edge to what it owns and sends to Core, its plant
// claims, its bins watch list and its cell catalog (shingo-edge engine
// feeds.go sendDue).
//
// A key is converging while each quote is the sender's current value or a copy
// sent after the copy the previous quote proved. A quote is true of the moment
// it was written, so the copies sent since are kept (Record). Remembering only
// the last send flagged receivers that applied every copy, and believed one
// frozen at the last push for ever. Pinned by
// TestRecord_AQuoteProvesOneSendAndWhatPrecededIt and, in shingo-core
// messaging, TestFeedReplay_TheSavedRunsFlagNothing.
//
// It is shared because the two copies drifted: the rule that a receiver
// quoting what it was sent has converged (so a value that moves on every
// heartbeat is not "stuck") was found on Core and mirrored to the Edge by
// hand, and the Edge's copy read the wall clock where Core's read its book's.
// Each caller keeps what is its own: where the records live, the flag text,
// the log line, and which of its sends are pushes (Pushed).
package resend

import (
	"slices"
	"time"
)

const (
	// Guard: a send of the same digest of the same key this recently is held
	// back. The copy is in flight, and the next heartbeat may simply predate
	// its arrival. It also bounds a key that never converges at one send per
	// Guard.
	Guard = 2 * time.Minute
	// FlagAfter is how many of Send's sends in a row, with no quote proving a
	// copy sent since the last one proved, mark a key as not converging.
	FlagAfter = 3
	// sentKept bounds the copies a Record keeps since the last one proved: the
	// newest, so a receiver further behind than this is counted as stuck. The
	// deepest seen in four saved rig runs was 83.
	sentKept = 256
)

// Record is what was sent of one key to one receiver since the receiver last
// proved one. The zero value is a key never sent.
type Record struct {
	// digest and at are Send's last, which the guard reads. A push leaves them
	// alone, so a lost push does not hold back the send that heals it.
	digest string
	at     time.Time
	// last and lastAt are the newest send by either path. They outlive its proof.
	last   string
	lastAt time.Time
	// sent is the digests sent by either path since the last one proved, oldest
	// first, at most sentKept.
	sent   []string
	streak int
	// held: the receiver has quoted the last send since Send's last, or the
	// sender's current value: nothing it was sent is on its way.
	held bool
}

// Quoted takes the receiver's quote d and reports whether it proves a send:
// one made since the last proved. Then the streak ends, and that send and
// every one before it is forgotten, so the same quote proves nothing twice and
// a frozen receiver still flags. Of a digest sent more than once the newest is
// taken, or a value going A, B, A would credit a receiver frozen at A once per
// A. Only a quote of the last send, Send's or the newest by either path, says
// the receiver holds it (held): short of that a copy is in flight, and the
// guard keeps it from going twice.
func (r *Record) Quoted(d string) bool {
	for i := len(r.sent) - 1; i >= 0; i-- {
		if r.sent[i] != d {
			continue
		}
		r.sent = slices.Delete(r.sent, 0, i+1)
		r.streak = 0
		if d == r.digest || d == r.last {
			r.held = true
		}
		return true
	}
	return false
}

// Converged records that the receiver holds the sender's current value: the
// streak ends, and a later quote of anything else is the receiver having lost
// it. Of the sends only the newest is still kept: the older are behind what
// the receiver holds.
func (r *Record) Converged() {
	r.streak, r.held = 0, true
	if n := len(r.sent); n > 1 {
		r.sent = slices.Delete(r.sent, 0, n-1)
	}
}

// Pushed records a send made outside Send (a push after a change, a save's
// publish): a quote of it converges like a quote of Send's. The guard and the
// streak are Send's alone and are untouched.
func (r *Record) Pushed(digest string, now time.Time) {
	r.last, r.lastAt = digest, now
	n := len(r.sent)
	if digest == "" || (n > 0 && r.sent[n-1] == digest) {
		return
	}
	if n == sentKept {
		r.sent = slices.Delete(r.sent, 0, 1)
	}
	r.sent = append(r.sent, digest)
}

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
	r.Pushed(digest, now) // on its way, as a push is
	return true
}

// Streak is the sends in a row since the key last converged.
func (r *Record) Streak() int { return r.streak }

// NotConverging reports whether the streak has reached FlagAfter.
func (r *Record) NotConverging() bool { return r.streak >= FlagAfter }

// Last is the newest send by either path and when, proved or not; "" and zero
// before any send.
func (r *Record) Last() (digest string, at time.Time) { return r.last, r.lastAt }
