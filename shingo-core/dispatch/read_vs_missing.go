package dispatch

import (
	"database/sql"
	"errors"
	"fmt"
)

// read_vs_missing.go — "that node does not exist" and "the database did not
// answer" are different facts, and for a long time this codebase could not tell
// them apart.
//
// Several sites read a node and wrote `if err != nil || node.ParentID == nil`,
// mapping both to a terminal. One of those is real geometry — somebody
// configured a lane that is not there, and no amount of retrying invents it. The
// other is a hiccup, and killing demand for it is the wait-not-fail rule broken
// on I/O rather than on congestion.
//
// The split is mechanical: the store's node getters return sql.ErrNoRows for
// absent and anything else for a read that failed (store/nodes ScanNode). What
// was missing was the intent to ask.
//
// The dispositions differ in both directions:
//
//   - READ FAILED → park, under a cause of its own. Not lane-busy: during an
//     outage dozens of orders park at once, and rendering that as congestion
//     sends an operator to look at lanes. Releaser: every one of these sites is
//     scanner-reachable, so the ordinary retry loop re-drives them and a read
//     that failed once usually succeeds next time.
//   - GENUINELY ABSENT → terminal, and the message is an instruction. Fixing
//     configuration is a human's job; the error's job is to say so, in words that
//     say what to go and fix rather than naming an internal code.

// readFailed reports whether an error from a node/row lookup is a FAILED READ
// rather than a genuine absence.
//
// A nil error is not a failure. sql.ErrNoRows is not a failure either — it is the
// answer "there is nothing there", which is exactly the terminal case.
func readFailed(err error) bool {
	return err != nil && !errors.Is(err, sql.ErrNoRows)
}

// configFailure renders the operator-facing message for something Core was told
// to use and cannot find.
//
// The wording is deliberate and uniform across the sites that use it. "config
// failure" says whose problem it is, the KIND says what sort of thing is
// missing, and the IDENTIFIER says which one — because the first question anyone
// asks is "which lane?" and an error that cannot answer it sends them reading
// logs instead of fixing the configuration.
func configFailure(kind, identifier string) string {
	return fmt.Sprintf("config failure: %s %s does not exist", kind, identifier)
}

// configFailureID is configFailure for the sites that hold an id rather than a
// name — a lane resolved from a bin's node_id, say. An id is still findable; a
// blank is not.
func configFailureID(kind string, id int64) string {
	return configFailure(kind, fmt.Sprintf("id %d", id))
}
