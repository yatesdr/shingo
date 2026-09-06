// Package material contains the pure logic that maps a bin's movement
// across a CMS boundary into a set of CMS transaction records.
//
// This package deliberately does not persist anything and does not
// emit any events. It takes a narrow Store (read-only, declared
// consumer-side in store.go), walks the node tree to find the CMS
// boundary, and returns a slice of *cms.Transaction values that
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
// # Boundary walking
//
// FindCMSBoundary walks the parent chain from a node looking for the
// nearest synthetic ancestor carrying the "cms_storeroom" property, and
// returns that node together with the storeroom code the property holds.
//
// One property, no defaults, fail-closed at every depth: presence makes a
// node a boundary and says which storeroom it is, absence means it is not
// one. A node is never a boundary because of where it sits in the tree.
//
// That last point is the whole reason this shape exists. The predicate it
// replaced defaulted parentless synthetic nodes ON, and nothing wrote the
// property in production — so the default WAS the behaviour, and every
// parentless synthetic node was a CMS boundary: _TRANSIT, every node group,
// and every per-robot carrier node. A bin picked up by a robot crossed from
// its real storeroom into "the robot", and the transfer was booked.
//
// If no tagged ancestor is found the walk returns (nil, "", nil). If it
// encounters a cycle or a Store error it returns (nil, "", err), because
// "the lookup failed" and "there is no boundary here" are different answers
// and a caller that cannot tell them apart records nothing for a real move.
package material
