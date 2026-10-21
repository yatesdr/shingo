package bins

import "database/sql"

// StandsUnheld reports whether the bin still stands on the node named
// nodeName and no order holds it — no hard claim, no lock, no active soft hold
// (BinUnheldSQL). It is the home clear's re-run state read
// (dispatch/clear_home.go): the empty the clear declared is on the home and
// nobody is coming for it. One statement.
func StandsUnheld(db *sql.DB, binID int64, nodeName string) (bool, error) {
	var ok bool
	err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM bins b JOIN nodes n ON n.id = b.node_id
		WHERE b.id = $1 AND n.name = $2 AND `+BinUnheldSQL+`)`, binID, nodeName).Scan(&ok)
	return ok, err
}
