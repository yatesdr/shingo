package domain

import (
	"hash/fnv"
	"strconv"
)

// cell_picture_version.go — the token the station poll carries INSTEAD of the
// cell picture.
//
// WHY THE PICTURE LEFT THE POLL (owner, 2026-09-17: "maps don't change often,
// especially near process nodes… why is this info flying over the wire so
// much?"). The picture rode every station view: two queries per poll per board
// — the process's whole node list and a partner-slot scan over every live
// claim — plus a deep copy of the plant's NGRP map taken under the core-node
// lock, all of it rebuilt at 500 ms a board while events flow, on a Pi with one
// SQLite connection. What it draws changes when an engineer edits a cell, which
// is a few times a week.
//
// So the poll carries this string and the page fetches the picture only when
// the string stops matching the one it holds.
//
// DERIVED, NOT ANNOUNCED, and that is the ruling (option B over option C). An
// announcement-only scheme is correct exactly as long as the event stream is,
// and the event stream has stalled in production before — an SSE stall at
// Springfield on 2026-08-19 killed its own refresh loop. A version derived from
// the facts cannot go stale on a missed message: every input below is either
// read on the poll already or published by the writer that changes it.
//
// THE FOUR INPUTS, AND WHY EACH IS FREE ON THE POLL:
//
//   - the running style — the view's own Process row.
//   - the live claims — newStationView already reads every one of them for the
//     picker block (liveClaimsByStyle). They decide the choreography drawn on
//     each card, the back-position captions, and which two groups the dock
//     strip names.
//   - the node generation — process_nodes is the one picture input the poll
//     does NOT read, so it is published instead by every writer of that table
//     (store/processes.BumpNodeGeneration).
//   - the plant generation — the geometry cache and the NGRP map, both replaced
//     only by a node-list response, both published by the engine on the same
//     counter.
//
// AND THE STATION ID, because a cell picture is station-scoped: two boards of
// one cell draw different position sets, and one version for both would hand a
// board the other one's picture.

// CellPictureVersionInput is everything CellPictureVersion reads. Every field
// is in hand on a station poll before this is called; see the file comment.
type CellPictureVersionInput struct {
	StationID     int64
	ProcessID     int64
	ActiveStyleID int64
	// NodeGeneration is store/processes.NodeGeneration() — bumped by every
	// writer of process_nodes.
	NodeGeneration uint64
	// PlantGeneration is the engine's counter for the two caches a node-list
	// response replaces: the scene geometry and the NGRP membership.
	PlantGeneration uint64
	// Claims is every LIVE claim of the process, in any order.
	Claims []NodeClaim
}

// CellPictureVersion is the token. Equal strings mean the picture has not
// moved; different strings mean it may have, and the page fetches.
//
// XOR OVER PER-CLAIM HASHES, NOT A RUNNING HASH OVER THE LIST. The claims
// arrive from liveClaimsByStyle, which groups them into a Go map — and Go
// randomises map iteration, so a sequential hash would produce a different
// string on every poll of a cell nobody touched. Every board would then fetch
// its picture twice a second, which is worse than the thing this replaced. XOR
// is commutative, so the order cannot reach the answer.
//
// THE COUNT TRAVELS BESIDE THE XOR because XOR alone cannot see a pair of
// identical claims arriving or leaving together. Two claims are never
// byte-identical in practice (the style id is in each one), but a fold whose
// correctness rests on "in practice" is one nobody can check.
func CellPictureVersion(in CellPictureVersionInput) string {
	var claimsXOR uint64
	for i := range in.Claims {
		claimsXOR ^= claimFingerprint(&in.Claims[i])
	}
	h := fnv.New64a()
	writeUint(h, uint64(in.StationID))
	writeUint(h, uint64(in.ProcessID))
	writeUint(h, uint64(in.ActiveStyleID))
	writeUint(h, in.NodeGeneration)
	writeUint(h, in.PlantGeneration)
	writeUint(h, uint64(len(in.Claims)))
	writeUint(h, claimsXOR)
	return strconv.FormatUint(h.Sum64(), 36)
}

// claimFingerprint is one claim's contribution: the fields the picture draws,
// plus the two that decide the dock strip's groups, plus the style and node
// that say WHICH claim this is.
//
// IT IS CellClaim's FIELD LIST, DELIBERATELY. The picture carries exactly
// these columns (cell_picture.go), so a column added there without being added
// here would be a picture that can change while its version does not — which
// is the one failure this whole mechanism has. The style id is included as
// well, because a claim moved from one style to another is a different picture
// the moment either style is running.
func claimFingerprint(c *NodeClaim) uint64 {
	h := fnv.New64a()
	writeUint(h, uint64(c.StyleID))
	// FIELD BY FIELD, NOT A LIST OF THEM. The obvious shape is a []string{...} of
	// every column, and the two index positions inside it are exactly the literal
	// protocol/claim_geometry_drift_test.go exists to refuse: a third position
	// added to the layout would have to find this site, which is what
	// ExtensionPositions was written to end.
	//
	// ExtensionPositions is not the answer here either, and the drift test's own
	// message says why - "if this is genuinely a POSITIONAL read it should not be
	// a list at all". It drops blanks, so ("PLN_02", "") and ("", "PLN_02") both
	// reduce to one name: two different claims, one hash, which on a fingerprint
	// is a picture that changes while its version does not. Each column
	// contributes in its own place, so it is written as places.
	//
	// The NUL keeps two adjacent fields from sliding into each other: ("AB", "")
	// and ("A", "B") are different claims and must not hash the same. It cannot
	// appear in a node name or a payload code.
	field := func(v string) {
		h.Write([]byte(v))
		h.Write([]byte{0})
	}
	field(c.CoreNodeName)
	field(string(c.SwapMode))
	field(c.PayloadCode)
	field(c.PairedCoreNode)
	field(c.SecondPairedCoreNode)
	field(c.InboundStaging)
	field(c.OutboundStaging)
	field(c.InboundSource)
	field(c.OutboundDestination)
	// Not a CellClaim column, and it belongs here anyway: the picture's position
	// SET is "bound to this station, or named by a claim", and a claim's role is
	// what the composer derives a fresh cell's role from.
	field(string(c.Role))
	// The chosen waypoints, which the picture marks on the leg. Each is its own
	// write for the same reason the columns are: joined into one string they
	// would let ["LM1","LM13"] and ["LM11","L3"] hash alike. The ORDER is the
	// route - LM167 then LM9 and LM9 then LM167 are different drives - so they
	// are written in order, with the count behind them.
	for _, lm := range c.KeyRoute {
		field(lm)
	}
	writeUint(h, uint64(len(c.KeyRoute)))
	return h.Sum64()
}

func writeUint(h interface{ Write([]byte) (int, error) }, v uint64) {
	var b [8]byte
	for i := range b {
		b[i] = byte(v >> (8 * i))
	}
	h.Write(b[:])
}

// BackPositionNames is every position any live claim of a process names as a
// partner slot — paired, second paired, inbound or outbound staging. An unused
// position is captioned "back position" from this set, because a cell's back
// slots are back slots under every style and not only under the running one.
//
// IT REPLACED A QUERY (store.ListBackPositionNames), and the reason is that
// every caller already had the rows. The old shape read the same
// style_node_claims table a second time, with its own JOIN, on the poll and on
// the desktop composer read — both of which had just listed the process's live
// claims for something else.
//
// LIVE CLAIMS, not just live styles, which is the rule the query learned the
// hard way: a position a flow used to stage through kept its "back position"
// caption after the save that dropped it. The caller passes live claims, and
// "live" is the store's predicate, not a filter re-implemented here.
func BackPositionNames(claims []NodeClaim) map[string]bool {
	out := map[string]bool{}
	for i := range claims {
		c := &claims[i]
		for _, name := range c.ExtensionPositions() {
			out[name] = true
		}
		for _, name := range []string{c.InboundStaging, c.OutboundStaging} {
			if name != "" {
				out[name] = true
			}
		}
	}
	return out
}
