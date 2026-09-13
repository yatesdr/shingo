// Package capacity resolves a claim's UOP capacity.
//
// It lives under store/internal because both store/processes (which reads
// claims) and the outer store/ (which re-exports it for the sim's own claim
// query) need it, and a store sub-package may not import a sibling aggregate —
// cross-aggregate sharing goes through store/internal. See the
// store-sub-pkg-isolation rule in .golangci.yml.
package capacity

import (
	"log"
	"strconv"
	"sync"
)

// A CLAIM'S CAPACITY IS CORE'S NUMBER, AND IT IS READ, NOT COPIED.
//
// `style_node_claims.uop_capacity` used to hold a copy, filled by the admin
// editor from the payload catalog at save time. Nothing re-copied it, so when Core's
// number moved the claim kept the old one until somebody happened to re-save
// the style. At Hopkinsville on 2026-09-03 that had already happened to the
// style that was RUNNING (Press 400 style 10: 10560 stored against a catalog
// saying 2500), which is a bin rendering as a quarter full forever and a
// "bin is full" demand edge that can never stamp.
//
// So capacity is resolved on every claim read instead — keyed on the payload
// code the claim binds, from the catalog Core syncs. The column stays in the
// table, dead, and nothing writes it.
//
// IT IS SQL, NOT A PER-READER LOOKUP, for one reason: the claim reads it hangs
// off are the ones the per-process publish work made single-query. A Go
// resolver called by each of the ten readers would put a catalog round trip
// back on every claim — and the station view, the claim API and the
// plant-claims publisher would each need a database handle in a path that has
// none. Resolved in the SELECT, every reader of NodeClaim.UOPCapacity gets
// Core's number with no round trip and no code of its own.

// Unknown is what the resolution answers when the catalog has no row
// for the claim's payload code. It is distinct from a catalog capacity of 0 so
// that "we do not know" can be logged and a genuine zero cannot be.
const Unknown = -1

// SQL is the SELECT-list expression that resolves a claim's UOP
// capacity. table is the claim table's name or alias as the surrounding query
// spells it, because the correlated subquery has to name the outer row.
//
// Pair it with Resolved, which turns the raw column into the number
// readers use.
func SQL(table string) string {
	return `COALESCE((SELECT pc.uop_capacity FROM payload_catalog pc WHERE pc.code = ` +
		table + `.payload_code), ` + strconv.Itoa(Unknown) + `)`
}

// unknownPayloadLogged keeps the "catalog does not know this payload" warning
// to once per code. The claim reads below it run on every publish and every
// operator action, so logging each one would be a line per read.
var unknownPayloadLogged sync.Map

// Resolved normalises what SQL scanned. An unknown payload
// answers 0 — the same "no capacity known" every reader already handles, and
// never a guess — and says so once.
//
// An empty payload code is not a complaint: manual_swap claims carry their
// switchable set in allowed_payload_codes and bind no single payload, so there
// is nothing to look up and nothing wrong.
func Resolved(raw int, payloadCode string) int {
	if raw != Unknown {
		return raw
	}
	if payloadCode == "" {
		return 0
	}
	if _, seen := unknownPayloadLogged.LoadOrStore(payloadCode, struct{}{}); !seen {
		log.Printf("catalog: no payload_catalog row for %q — capacity reads 0 until Core syncs it", payloadCode)
	}
	return 0
}
