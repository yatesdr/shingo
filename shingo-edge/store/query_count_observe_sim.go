//go:build sim

package store

// SetStatementObserver installs fn as the observer of every counting store's
// statements (statementObserver, query_count.go); nil removes it. One observer
// per process: the sim Edge opens one store, and its statement report
// (cmd/shingoedge/sim_statements.go) names and tags each statement through it.
// Sim builds only, so nothing outside a sim build can set the observer.
func SetStatementObserver(fn func(query string)) {
	if fn == nil {
		statementObserver.Store(nil)
		return
	}
	statementObserver.Store(&fn)
}
