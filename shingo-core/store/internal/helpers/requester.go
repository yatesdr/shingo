package helpers

import "shingocore/domain"

// RequesterColumn is the column of orders or mission_telemetry a requester
// lives in: requested_by for a Core actor, station_uid for anything else (a
// station's uid, or the blank). A non-blank requester can live in only one of
// the two (v185's CHECKs), so for any r but the blank `<RequesterColumn(r)> = r`
// matches exactly the rows `station_id = r` matched before v185 split the
// actors out. The blank is not exact: a Core actor's orders have a blank
// station_uid too, so matching the blank would take them in. Every caller treats a blank requester as unscoped and never
// filters by it (the filters' `!= ""` guards; unlistedFor returns early).
func RequesterColumn(r string) string {
	if domain.IsCoreActor(r) {
		return "requested_by"
	}
	return "station_uid"
}

// RequesterSQL is the requester of the orders or mission_telemetry row named
// by alias (no alias: the table's own columns): its station_uid, or its
// requested_by when the station is blank. It is the value station_id held
// before v185, for a reader that groups or addresses by it.
func RequesterSQL(alias string) string {
	p := ""
	if alias != "" {
		p = alias + "."
	}
	return "CASE WHEN " + p + "station_uid <> '' THEN " + p + "station_uid ELSE " + p + "requested_by END"
}
