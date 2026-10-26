package service

import (
	"database/sql"
	"log"
	"time"

	"shingocore/store/audit"
)

// fifo_date.go — a correction does not cost a carrier its place in line.
//
// Every confirm stamped a new loaded_at, the key FIFO ranks carriers by, and
// every clear blanked it. So a carrier whose count was corrected by clearing it
// and setting the same part again, or topped up with the same part, ranked as
// the newest stock of its part while its parts were the same old ones.
// Hopkinsville's 4x2 supermarket carts were cleared and set again at the same
// count five times in two days, 4 seconds to 13 minutes apart (2026-10-21/22).

// correctionWindow bounds a clear and its re-add as one correction. The
// longest HK correction took 13 minutes; past this, a carrier cleared with
// parts could have been emptied and refilled with fresh ones.
const correctionWindow = 20 * time.Minute

// keepFIFODate decides whether a confirm keeps the carrier's earlier FIFO date
// rather than stamping now. rows are the bin's newest carrier rows
// (audit.RecentCarrierRows), newest first; the first must be the
// set_for_production this confirm completes. It answers the date to keep and
// why, or nil, and a nil is today's behaviour: the confirm stamps now.
//
//   - Case 1, the same part set again without a clear (a top-up, or a count
//     fixed by setting it again): the carrier held that part with parts in it.
//   - Case 2, a clear and the same part set again: the carrier held parts at the
//     clear, it is set again at the same spot with nothing between, within
//     correctionWindow of the clear.
//
// Anything else is new stock: a different part, a carrier that moved, was
// lifted or released between, or a re-add past the window. A carrier released
// at a count of 0 and set again looks exactly like a refill, so it is new stock
// too.
func keepFIFODate(rows []audit.CarrierRow) (*time.Time, string) {
	if len(rows) < 2 || rows[0].Op != audit.OpSetForProduction || rows[0].Payload == "" {
		return nil, ""
	}
	set := rows[0]
	switch prev := rows[1]; prev.Op {
	case audit.OpManifestConfirmed:
		if prev.Payload == set.Payload && positive(set.BeforeUOP) {
			return dateConfirmed(prev), "the same part set again on a carrier that still held it"
		}
	case audit.OpClearForReuse:
		if len(rows) < 3 {
			return nil, ""
		}
		conf := rows[2]
		if conf.Op == audit.OpManifestConfirmed && conf.Payload == set.Payload &&
			positive(prev.BeforeUOP) && sameNode(prev.NodeID, set.NodeID) &&
			!set.At.Before(prev.At) && set.At.Sub(prev.At) <= correctionWindow {
			return dateConfirmed(conf), "cleared and set again with the same part at the same spot within 20 minutes"
		}
	}
	return nil, ""
}

// keptLoadedAt reads the bin's history for keepFIFODate, on the confirm's own
// transaction. An explicit production time is the date and is never replaced.
// A history that cannot be read stamps now, as every confirm did.
func keptLoadedAt(tx *sql.Tx, binID int64, producedAt string) (*time.Time, string) {
	if producedAt != "" {
		return nil, ""
	}
	rows, err := audit.RecentCarrierRows(tx, binID, 3)
	if err != nil {
		log.Printf("bin_manifest: confirm bin %d: its history for the FIFO date is unreadable (%v) — stamping now", binID, err)
		return nil, ""
	}
	return keepFIFODate(rows)
}

// dateConfirmed is the date a confirm row stamped: its detail's, else the row's
// own time (a confirm written before the detail carried it, which stamped now).
func dateConfirmed(r audit.CarrierRow) *time.Time {
	if r.LoadedAt != nil {
		return r.LoadedAt
	}
	at := r.At
	return &at
}

func positive(n *int) bool { return n != nil && *n > 0 }

func sameNode(a, b *int64) bool { return a != nil && b != nil && *a == *b }
