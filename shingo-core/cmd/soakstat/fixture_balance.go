package main

import (
	"database/sql"
	"fmt"
	"strings"
)

// [S.2] FIXTURE BALANCE: is each payload made as fast as it is drawn?
//
// A sim fixture that makes a payload faster than its stations draw it fills its
// carrier pool with fulls, and then every order that needs an empty of that type
// waits, and every market slot the fulls take is gone. That is the honest state
// of an unbalanced plant, and on the demo fixture it read as a Core jam twice
// (2026-10-09: all 18 STANDARD-SM carriers full of PANEL-B, SYN_MARKET 41/45
// with PANEL-A). So every run says, per payload, what it made and drew, against
// the carrier pool it rides, and flags an imbalance before anyone reads the
// stranded table as Core's (RULINGS-C2b-second-jam-and-sim-stabilisation, item 3).
//
// MEASURED, NOT NOMINAL. simcalc prices a weld at its nameplate rate, and in this
// rig a weld cycles only while every carrier it needs is in place, so its
// effective draw is about a third of nameplate: on nameplate the demo fixture was
// "balanced" while its presses made two and 1.7 times what the welds drew. The
// unit ledger's produce and consume ticks are the stations' own count of what
// they did, so this reads them.
//
// MADE IS ATTRIBUTED BY THE PRODUCING NODE. A produce tick carries the carrier's
// payload code only while the carrier is labelled, and a press ticks into an
// empty carrier: on the 8a2b01e4 demo 1080 of PLN_001/003/004's 1170 produce
// ticks carried no code. So each produce node's ticks count for the payload its
// labelled ticks name (PLN_001 PANEL-A, PLN_003/004 PANEL-B); a tick's own code
// wins where it has one, and a node with no labelled tick is not counted.
//
// A payload is flagged when it was made more than balanceOver faster than drawn,
// or drawn more than balanceOver faster than made, by at least one carrier's
// worth (its uop_capacity) either way, so a run that ends between two swaps is
// not called unbalanced on one bin. Both directions, because an under-made
// payload is the same fixture fault seen from the other side: its welds wait on
// fulls the presses cannot make, and that wait reads as a Core material wait
// just as the over-made pool's reads as a jam. Until 2026-10-10 only over-make
// was flagged. A payload with no consume tick (drained by an unloader, or
// delivered and never ticked) or no produce tick (supplied by a loader, made by
// no press in the rig) is reported, not judged. A flag is a fixture finding, not
// a Core failure: it does not fail the run, the stranded table decides that.
const (
	measureBalance = "[S.2] FIXTURE BALANCE"
	balanceOver    = 1.10
)

type payloadBalance struct {
	payload, binType string
	made, drawn      int
	capacity         int
	pool, fullOfIt   int
}

// over reports whether the payload was made faster than drawn past the margin.
func (b payloadBalance) over() bool {
	return b.drawn > 0 && float64(b.made) > float64(b.drawn)*balanceOver && b.made-b.drawn >= max(b.capacity, 1)
}

// under is over's mirror: drawn faster than made past the same margin and the
// same carrier's worth.
func (b payloadBalance) under() bool {
	return b.made > 0 && float64(b.drawn) > float64(b.made)*balanceOver && b.drawn-b.made >= max(b.capacity, 1)
}

func (b payloadBalance) flag() string {
	return fmt.Sprintf("FIXTURE: %s made %d, drawn %d (%+.0f%%) on %s's pool of %d carriers, %d full of %s at the end",
		b.payload, b.made, b.drawn, 100*(float64(b.made)/float64(b.drawn)-1), b.binType, b.pool, b.fullOfIt, b.payload)
}

// fixtureBalance reads each ticked payload's made and drawn units, its carrier
// type (the type most of its bins are), that type's pool, and how many of the
// pool hold the payload with stock at the end. One statement.
func fixtureBalance(db *sql.DB) ([]payloadBalance, error) {
	rows, err := db.Query(`
		WITH ticks AS (
			SELECT node_id, reason, payload_code, ABS(after_uop - before_uop) AS u
			FROM bin_uop_ledger WHERE reason IN ('produce_tick', 'consume_tick')),
		node_makes AS (
			SELECT DISTINCT ON (node_id) node_id, payload_code AS p
			FROM ticks WHERE reason = 'produce_tick' AND payload_code <> ''
			GROUP BY node_id, payload_code
			ORDER BY node_id, COUNT(*) DESC, payload_code),
		flow AS (
			SELECT COALESCE(NULLIF(t.payload_code, ''), nm.p) AS p,
			       COALESCE(SUM(t.u) FILTER (WHERE t.reason = 'produce_tick'), 0) AS made,
			       COALESCE(SUM(t.u) FILTER (WHERE t.reason = 'consume_tick'), 0) AS drawn
			FROM ticks t LEFT JOIN node_makes nm ON nm.node_id = t.node_id AND t.reason = 'produce_tick'
			WHERE COALESCE(NULLIF(t.payload_code, ''), nm.p) IS NOT NULL
			GROUP BY 1),
		carrier AS (
			SELECT DISTINCT ON (b.payload_code) b.payload_code AS p, b.bin_type_id AS t
			FROM bins b WHERE b.payload_code <> ''
			GROUP BY b.payload_code, b.bin_type_id
			ORDER BY b.payload_code, COUNT(*) DESC, b.bin_type_id)
		SELECT f.p, COALESCE(bt.code, '?'), f.made, f.drawn, COALESCE(pl.uop_capacity, 0),
		       (SELECT COUNT(*) FROM bins b WHERE b.bin_type_id = c.t AND b.status <> 'retired'),
		       (SELECT COUNT(*) FROM bins b WHERE b.bin_type_id = c.t AND b.payload_code = f.p
		          AND COALESCE(b.uop_remaining, 0) > 0)
		FROM flow f
		LEFT JOIN carrier c ON c.p = f.p
		LEFT JOIN bin_types bt ON bt.id = c.t
		LEFT JOIN payloads pl ON pl.code = f.p
		ORDER BY f.p`)
	if err != nil {
		return nil, fmt.Errorf("fixture balance: %w", err)
	}
	defer rows.Close()
	var out []payloadBalance
	for rows.Next() {
		var b payloadBalance
		if err := rows.Scan(&b.payload, &b.binType, &b.made, &b.drawn, &b.capacity, &b.pool, &b.fullOfIt); err != nil {
			return nil, fmt.Errorf("fixture balance: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// printBalance writes the section: every ticked payload, then its flags.
func printBalance(bal []payloadBalance, unrun bool) {
	fmt.Printf("%s", measureBalance)
	if unrun {
		fmt.Println("  FAILED-TO-RUN")
		fmt.Println()
		return
	}
	fmt.Printf("  made vs drawn per payload, flagged past %.0f%% either way and one carrier\n", 100*(balanceOver-1))
	fmt.Printf("        %-10s %-12s %7s %7s %10s\n", "payload", "carrier", "made", "drawn", "pool/full")
	var flags []string
	for _, b := range bal {
		drawn := fmt.Sprint(b.drawn)
		if b.drawn == 0 {
			drawn = "-"
		}
		fmt.Printf("        %-10s %-12s %7d %7s %6d/%-3d\n", b.payload, b.binType, b.made, drawn, b.pool, b.fullOfIt)
		if b.over() || b.under() {
			flags = append(flags, b.flag())
		}
	}
	if len(flags) == 0 {
		fmt.Println("        balanced: no payload made or drawn more than 10% faster than the other")
	}
	for _, f := range flags {
		fmt.Printf("        %s\n", f)
	}
	fmt.Println()
}

// balanceFlags is the one-line form for the summary: "fixture balanced" or the
// flagged payloads, each direction named.
func balanceFlags(bal []payloadBalance) string {
	var over, under []string
	for _, b := range bal {
		switch {
		case b.over():
			over = append(over, b.payload)
		case b.under():
			under = append(under, b.payload)
		}
	}
	var parts []string
	if len(over) > 0 {
		parts = append(parts, "over-makes "+strings.Join(over, ","))
	}
	if len(under) > 0 {
		parts = append(parts, "under-makes "+strings.Join(under, ","))
	}
	if len(parts) == 0 {
		return "fixture balanced"
	}
	return "FIXTURE " + strings.Join(parts, ", ")
}
