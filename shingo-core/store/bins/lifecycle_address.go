package bins

import (
	"strings"

	"shingocore/store/internal/helpers"
	"shingocore/store/plantclaims"
)

// LifecycleAddressSQL is the RETURNING tail that addresses a lifecycle
// announcement (a generation bump's, a count's) by the landing's rule for a
// placement with no order (Lane 0 item 5; RULINGS-C2c-certification §4.1).
// Three columns, read off the bin row as the statement leaves it:
//
//   - whether the bin's node is a place no station binds at
//     (helpers.NodeNotASeatSQL, the landing's own judgement);
//   - the stations claiming the node, comma-joined and sorted
//     (plantclaims.ClaimingStationsSQL, the landing's own read: claims and
//     positions);
//   - every registered Edge, comma-joined and sorted (plantclaims.LastArmEdgesSQL,
//     the landing's last arm). Read every time, not only when no station
//     claims the node: edge_registry is one row per Edge, and a CASE would
//     evaluate the claiming read twice.
//
// It rides the UPDATE each path already makes, so addressing costs no
// statement. Scanned by ScanLifecycleAddress, in this order; the service
// applies the rule (service.landingStations), so the landing and the
// announcements share one spelling on both sides.
var LifecycleAddressSQL = helpers.NodeNotASeatSQL + `,
	COALESCE((SELECT string_agg(cs.station_id, ',' ORDER BY cs.station_id)
	            FROM nodes an, LATERAL (` + plantclaims.ClaimingStationsSQL("an.name") + `) cs
	           WHERE an.id = bins.node_id), ''),
	COALESCE((SELECT string_agg(er.station_id, ',' ORDER BY er.station_id) ` + plantclaims.LastArmEdgesSQL + `), '')`

// LifecycleAddress is what LifecycleAddressSQL read: where a lifecycle
// announcement for the bin may go.
type LifecycleAddress struct {
	NotASeat bool
	Claiming []string
	Plant    []string
}

// ScanLifecycleAddress returns the scan destinations for LifecycleAddressSQL's
// three columns and a func that turns them into the address once scanned.
func ScanLifecycleAddress() (dest []any, address func() LifecycleAddress) {
	var notASeat bool
	var claiming, plant string
	return []any{&notASeat, &claiming, &plant}, func() LifecycleAddress {
		return LifecycleAddress{NotASeat: notASeat, Claiming: splitStations(claiming), Plant: splitStations(plant)}
	}
}

func splitStations(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}
