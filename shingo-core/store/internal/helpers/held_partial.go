package helpers

import "database/sql"

// HeldPartialSQL is the HELD predicate on bins row alias a: the carrier is
// unfinished (its part set, its count above 0 and below the part's capacity)
// and it stands inside a partial home of its own part — the home named, its
// group, or the group above its lane. One spelling, read by the arrival stamp
// and the clear's refusal, so the two cannot disagree about what is held.
func HeldPartialSQL(a string) string {
	return `(` + a + `.payload_code <> '' AND ` + a + `.uop_remaining > 0
	  AND ` + a + `.uop_remaining < COALESCE((SELECT p.uop_capacity FROM payloads p WHERE p.code = ` + a + `.payload_code), 0)
	  AND ` + InPartialHomeSQL(a+".node_id", a+".payload_code") + `)`
}

// InPartialHomeSQL is the HOME half of the predicate: the node (an SQL
// expression) stands inside a partial home of the part (another) — the home
// named, its group, or the group above its lane.
func InPartialHomeSQL(nodeExpr, payloadExpr string) string {
	return `EXISTS (SELECT 1 FROM style_claims sc JOIN nodes h ON h.name = sc.partial_destination
	              WHERE sc.payload_code = ` + payloadExpr + ` AND sc.role = 'produce' AND sc.partial_destination <> ''
	                AND (h.id = ` + nodeExpr + `
	                     OR h.id = (SELECT n1.parent_id FROM nodes n1 WHERE n1.id = ` + nodeExpr + `)
	                     OR h.id = (SELECT n2.parent_id FROM nodes n1 JOIN nodes n2 ON n2.id = n1.parent_id
	                                WHERE n1.id = ` + nodeExpr + `)))`
}

// StampHeldSinceTx writes a carrier's held_since as it lands: now, when it is an
// unfinished carrier standing in a partial home of its part; NULL anywhere else.
// The placement (PlaceBinTx) calls it for every landing.
func StampHeldSinceTx(ex interface {
	Exec(string, ...any) (sql.Result, error)
}, binID int64) error {
	_, err := ex.Exec(`UPDATE bins b SET held_since = CASE WHEN `+HeldPartialSQL("b")+` THEN NOW() ELSE NULL END
		WHERE b.id = $1`, binID)
	return err
}

// StampHeldSinceDeclaredTx is the declaration's form: a count a person declares
// at the carrier's node makes it held from now, unless it already was: a
// recount keeps its place in the call-back order. NULL when it is not held.
func StampHeldSinceDeclaredTx(ex interface {
	Exec(string, ...any) (sql.Result, error)
}, binID int64) error {
	_, err := ex.Exec(`UPDATE bins b SET held_since = CASE WHEN `+HeldPartialSQL("b")+` THEN COALESCE(b.held_since, NOW()) ELSE NULL END
		WHERE b.id = $1`, binID)
	return err
}
