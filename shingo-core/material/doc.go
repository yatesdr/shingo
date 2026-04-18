// Package material contains the pure logic that maps a bin's
// movement or manifest correction into a set of CMS transaction
// records.
//
// This package deliberately does not persist anything and does not
// emit any events. It takes a narrow Store (read-only, declared
// consumer-side in store.go), walks the node tree to find the CMS
// boundary, and returns a slice of *store.CMSTransaction values that
// the caller is free to write to the database however it likes. If
// nothing needs to be recorded the functions return a nil slice and
// nil error — "no rows" is never an error.
//
// Persistence (store.DB.CreateCMSTransactions) and event emission
// (EventCMSTransaction on the engine bus) live in the engine wrapper
// in engine/cms_transactions.go. The intent is that the material
// package be unit-testable without spinning up an engine or a
// database: drop a hand-rolled fake store into the call and inspect
// the returned transactions.
//
// Boundary walking
//
// FindCMSBoundary walks the parent chain from a node looking for the
// nearest synthetic ancestor whose "log_cms_transactions" property
// enables CMS logging. Defaults:
//
//   - a parentless (root) synthetic node is enabled unless the
//     property is explicitly "false";
//   - any other synthetic node is disabled unless the property is
//     explicitly "true".
//
// If no synthetic ancestor is found the walk returns (nil, nil); if
// the walk encounters a cycle or a Store error it returns
// (nil, err) so callers can log the failure separately from the
// normal "no boundary here" case.
package material
