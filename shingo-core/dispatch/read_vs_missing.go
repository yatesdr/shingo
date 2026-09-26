package dispatch

import "shingocore/dispatch/plan"

// read_vs_missing.go — "that node does not exist" and "the database did not
// answer" are different facts. The distinction, and the one renderer and reader
// of the operator's "config failure" message, moved into the planner with the
// picker that asks it first (plan/read_vs_missing.go, B1); dispatch keeps its
// names for the sites here that ask it too.

// readFailed reports whether an error from a node/row lookup is a FAILED READ
// rather than a genuine absence. See plan.ReadFailed.
func readFailed(err error) bool { return plan.ReadFailed(err) }

// configFailure renders the operator-facing message for something Core was told
// to use and cannot find. See plan.ConfigFailure.
func configFailure(kind, identifier string) string { return plan.ConfigFailure(kind, identifier) }

// configFailureID is configFailure for the sites that hold an id rather than a
// name. See plan.ConfigFailureID.
func configFailureID(kind string, id int64) string { return plan.ConfigFailureID(kind, id) }

// IsConfigFailure reports whether a terminal detail was written by
// configFailure — whether the thing that stopped is a human's to fix. See
// plan.IsConfigFailure.
func IsConfigFailure(detail string) bool { return plan.IsConfigFailure(detail) }
