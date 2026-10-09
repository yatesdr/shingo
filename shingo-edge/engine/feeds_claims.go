package engine

import "strings"

// feeds_claims.go — the Edge-owned direction of the feed digests: this Edge's
// plant claims, one digest per process, which Core quotes back on the ack.
// The publisher lives in messaging and does the reading and sending; the
// engine keeps the resend guard (sendDue / converged in feeds.go, the same
// rule Core applies to its feeds) and hands the ack's Claims map over.

// claimsKeyPrefix scopes the guard's keys for plant claims: "claims:<process>".
const claimsKeyPrefix = "claims:"

// SetClaimsAckFunc injects the publisher's reconcile, called with the Claims
// map of every ack that carries one. Set at the composition root before the
// first ack can arrive; the engine never imports the publisher.
func (e *Engine) SetClaimsAckFunc(fn func(claims map[string]string)) {
	e.claimsAckFn = fn
}

// answerClaims hands an ack's Claims to the publisher. nil is an older Core,
// or one that could not read its table: nothing is read and nothing is sent.
func (e *Engine) answerClaims(claims map[string]string) {
	if claims == nil || e.claimsAckFn == nil {
		return
	}
	e.claimsAckFn(claims)
}

// ClaimSendDue is the resend guard for one process's plant-claims report;
// quoted is the digest Core holds for the process ("" for none).
func (e *Engine) ClaimSendDue(process, digest, quoted string) bool {
	return e.sendDue(claimsKeyPrefix+process, digest, quoted)
}

// ClaimsSettled converges every process key the guard has seen that is not
// in pending: Core holds what this Edge would send, or the name is gone from
// both sides. A name that leaves both sides is never quoted again, so this is
// the only place its streak and flag can end.
func (e *Engine) ClaimsSettled(pending map[string]bool) {
	e.feeds.mu.Lock()
	var settled []string
	for key := range e.feeds.sent {
		if process, ok := strings.CutPrefix(key, claimsKeyPrefix); ok && !pending[process] {
			settled = append(settled, key)
		}
	}
	e.feeds.mu.Unlock()
	for _, key := range settled {
		e.converged(key)
	}
}
