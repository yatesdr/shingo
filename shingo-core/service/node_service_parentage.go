package service

import (
	"errors"

	"shingocore/store/nodes"
)

// IsParentCycle reports whether err is the store's refusal to create a parentage
// cycle, so an HTTP layer can answer 400 instead of 500 — the request is
// well-formed and the caller fixes it by choosing a different parent.
//
// It lives here because www may not import store packages (depguard's
// www-no-direct-store), and the alternative — matching on the error's text —
// is the kind of coupling that survives exactly until somebody improves the
// wording.
func IsParentCycle(err error) bool { return nodes.IsParentCycle(err) }

// IsSlotEnabledFollowsLane reports whether err is the store's refusal to switch
// one slot of a lane on or off, so an HTTP layer can answer 400 instead of 500
// — the form is well-formed and the operator fixes it by switching the LANE.
//
// It lives here for the same reason IsParentCycle does: www may not import
// store packages (depguard's www-no-direct-store), and the alternative —
// matching on the error's text — is the kind of coupling that survives exactly
// until somebody improves the wording.
//
// errors.Is rather than the sentinel bare: the store wraps it with the node id
// (`%w (node %d)`), so a bare == would answer false on every real refusal.
func IsSlotEnabledFollowsLane(err error) bool {
	return errors.Is(err, nodes.ErrSlotEnabledFollowsLane)
}
