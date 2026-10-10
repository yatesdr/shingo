package main

import (
	"database/sql"
	"fmt"
	"time"
)

// [S.3] LINESIDE DIVERGENCES: the run's lineside report episodes, per class.
//
// Core compares each 60-s Edge lineside report against its replica and records
// a disagreement as a report_divergence episode in bin_uop_exception, opened
// when first seen and recovered when a later report agrees
// (service/lineside_divergence.go). The episodes decide nothing and heal
// nothing, so a rising count or a long-open one is the only sign of drift. Every
// run says how many opened per class, how many closed, the longest, and how many
// are open at the collect, so the trend is visible run over run
// (RULINGS-C2b-second-jam-and-sim-stabilisation, item 4). On the 2026-10-09 demo
// runs every one closed after one report, and every count episode had the Edge
// one unit ahead of Core at the same seq (handed to the bucket-drift stream).
//
// Episodes are stamped with the report's time, on the sim clock, so an open one
// is aged against the same now the stranded table reads.
const measureDivergence = "[S.3] LINESIDE DIVERGENCES"

type divergenceClass struct {
	class          string
	opened, closed int
	longestMin     float64
}

// lineSideDivergences reads the episodes per class. One statement.
func lineSideDivergences(db *sql.DB, simNow *time.Time) ([]divergenceClass, error) {
	var now any
	if simNow != nil {
		now = simNow.UTC()
	}
	rows, err := db.Query(`
		WITH now_sim AS (SELECT COALESCE($1::timestamptz, (SELECT MAX(created_at) FROM order_history)) AS t)
		SELECT op, COUNT(*), COUNT(recovered_at),
		       COALESCE(MAX(EXTRACT(EPOCH FROM (COALESCE(recovered_at, (SELECT t FROM now_sim)) - occurred_at))) / 60.0, 0)
		FROM bin_uop_exception WHERE kind = 'report_divergence'
		GROUP BY op ORDER BY op`, now)
	if err != nil {
		return nil, fmt.Errorf("lineside divergences: %w", err)
	}
	defer rows.Close()
	var out []divergenceClass
	for rows.Next() {
		var c divergenceClass
		if err := rows.Scan(&c.class, &c.opened, &c.closed, &c.longestMin); err != nil {
			return nil, fmt.Errorf("lineside divergences: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// printDivergences writes the section.
func printDivergences(classes []divergenceClass, unrun bool) {
	fmt.Printf("\n%s", measureDivergence)
	if unrun {
		fmt.Println("  FAILED-TO-RUN")
		return
	}
	fmt.Println("  report episodes per class (they decide nothing; drift shows here)")
	if len(classes) == 0 {
		fmt.Println("        none")
		return
	}
	open := 0
	fmt.Printf("        %-16s %6s %6s %12s\n", "class", "opened", "closed", "longest(min)")
	for _, c := range classes {
		fmt.Printf("        %-16s %6d %6d %12.1f\n", c.class, c.opened, c.closed, c.longestMin)
		open += c.opened - c.closed
	}
	fmt.Printf("        open at the collect: %d\n", open)
}
