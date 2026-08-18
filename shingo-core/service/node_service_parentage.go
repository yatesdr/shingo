package service

import "shingocore/store/nodes"

// IsParentCycle reports whether err is the store's refusal to create a parentage
// cycle, so an HTTP layer can answer 400 instead of 500 — the request is
// well-formed and the caller fixes it by choosing a different parent.
//
// It lives here because www may not import store packages (depguard's
// www-no-direct-store), and the alternative — matching on the error's text —
// is the kind of coupling that survives exactly until somebody improves the
// wording.
func IsParentCycle(err error) bool { return nodes.IsParentCycle(err) }
