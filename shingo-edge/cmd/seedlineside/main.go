// seedlineside - a dev-only tool: insert lineside bucket rows into an Edge
// sqlite database so the Production page's Lineside Buckets table has
// something to show. Buckets arise in production from the capture_lineside
// release disposition; the dev sim has no natural source of them, so a test
// of the table (and its clear button) needs them seeded by hand.
//
// Usage: go run ./cmd/seedlineside <db path> [<count>]
//
// Rows reference REAL nodes and payloads read from the same database (the
// first N nodes with a live claim's payload), so nothing dangles. Writes go
// through lineside.Capture, the pile table's one sanctioned write door, which
// ADDS to an existing pile's qty - so a re-run grows the piles rather than
// resetting them; clear them on the Production page between runs.
package main

import (
	"database/sql"
	"fmt"
	"os"
	"strconv"

	"shingoedge/store/lineside"

	_ "modernc.org/sqlite"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: seedlineside <db path> [count]")
		os.Exit(2)
	}
	count := 3
	if len(os.Args) > 2 {
		n, err := strconv.Atoi(os.Args[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, "count:", err)
			os.Exit(2)
		}
		count = n
	}
	db, err := sql.Open("sqlite", os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	defer db.Close()

	// The seed targets: real process nodes, named, with a payload each claims.
	rows, err := db.Query(`SELECT n.id, n.core_node_name, c.payload_code
		FROM process_nodes n
		JOIN style_node_claims c ON c.core_node_name = n.core_node_name
		WHERE c.payload_code <> '' AND c.retired_at IS NULL
		AND c.style_id IN (SELECT id FROM styles WHERE deleted_at IS NULL)
		GROUP BY n.id
		ORDER BY n.id LIMIT ?`, count)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read nodes:", err)
		os.Exit(1)
	}
	type target struct {
		id      int64
		name    string
		payload string
	}
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.id, &t.name, &t.payload); err != nil {
			fmt.Fprintln(os.Stderr, "scan:", err)
			os.Exit(1)
		}
		targets = append(targets, t)
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "rows:", err)
		os.Exit(1)
	}
	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "no nodes with claimed payloads found - seed the styles first")
		os.Exit(1)
	}

	for _, t := range targets {
		// THROUGH THE SANCTIONED VERB, not a raw INSERT: Capture is the pile
		// table's one write door (uop's pile-writer arch test pins the set),
		// and a capture-shaped write is what the row is anyway. The level's
		// dirty-marking stays the engine verbs' business - a dev seed does
		// not need Core's mirror to follow it.
		if _, err := lineside.Capture(db, t.id, t.payload, 37); err != nil {
			fmt.Fprintf(os.Stderr, "seed %s/%s: %v\n", t.name, t.payload, err)
			os.Exit(1)
		}
		fmt.Printf("seeded %s: %s qty=37\n", t.name, t.payload)
	}
}
