package domain

import "slices"

// The Core actors: who asked for an order when no station did. Each is the
// sender name one of Core's own pages puts on the envelope it builds, or the
// name a door that writes an order directly stands in for:
//
//   - core-operator: Core's Orders and Bins pages (a Core-role envelope, and
//     the bin-move door);
//   - core-direct: the test-orders page's direct tab (an Edge-role envelope
//     handed to the dispatcher in process, and the bin-move door);
//   - core-test: the test-orders page's Kafka tab, which publishes as an Edge.
//
// They are the only values orders.requested_by and mission_telemetry.requested_by
// hold, and the values a station column never holds (v185's CHECKs). No Edge's
// uid is one of them, so a message addressed to one reaches no Edge.
const (
	RequesterOperator = "core-operator"
	RequesterDirect   = "core-direct"
	RequesterTest     = "core-test"
)

// CoreActors lists the Core actors, the one list IsCoreActor reads. v185
// keeps its own copy (store's v185Actors), since a migration's text must not
// follow a later change to this one.
var CoreActors = []string{RequesterOperator, RequesterDirect, RequesterTest}

// IsCoreActor reports whether s is one of the Core actors.
func IsCoreActor(s string) bool { return slices.Contains(CoreActors, s) }

// SplitRequester reads an envelope's sender as the two columns an order keeps:
// a Core actor's name is the requested_by, with no station; anything else,
// blank included, is the station's uid. Intake calls it once, on the sender
// (dispatch's order request and complex order request); Engine.CreateBinMove
// calls it on a caller that names its actor as the station; and
// service.BinService.Lift calls it on the lifting order's requester, for the
// lifted ledger row's station and who.
func SplitRequester(sender string) (stationUID, requestedBy string) {
	if IsCoreActor(sender) {
		return "", sender
	}
	return sender, ""
}

// Requester is who asked for the order, for a label, a filter, an owner
// check or an address: its station's uid, or the Core actor when no station
// asked; "" for an order Core minted with neither. It is the value the
// station column held for every order before v185 split the actors out.
func (o *Order) Requester() string {
	if o.StationUID != "" {
		return o.StationUID
	}
	return o.RequestedBy
}
