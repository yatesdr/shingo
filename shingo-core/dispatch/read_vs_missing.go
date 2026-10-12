package dispatch

import "shingocore/dispatch/plan"

// read_vs_missing.go — "that node does not exist" and "the database did not
// answer" are different facts. The distinction, and the one renderer and reader
// of the operator's "config failure" message, live in the planner
// (plan/read_vs_missing.go). readFailed is its name for the dispatch sites where
// a local plan variable shadows the package.

// readFailed reports whether an error from a node/row lookup is a FAILED READ
// rather than a genuine absence. See plan.ReadFailed.
func readFailed(err error) bool { return plan.ReadFailed(err) }
