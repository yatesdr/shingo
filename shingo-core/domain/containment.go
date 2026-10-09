package domain

import "shingo/protocol"

// PayloadContainmentRow is one payload's containment state as the UI reads it.
// The row persists after deactivation so the alert's history survives it.
//
// The definition lives in protocol, because the same row travels to every Edge
// on the containment snapshot; this alias keeps the containment handlers' name
// (www may not import store) and makes GET /api/containment and the snapshot one
// type, so their JSON cannot drift apart. store keeps the name through an alias
// of this alias.
type PayloadContainmentRow = protocol.PayloadContainmentRow

// HeldBinRow is one held bin as the containment screens read it. The bin's
// location is carried as the node name only: the screens render the name, and
// the Edge's copy of this row has no node id to read. Defined in protocol; see
// PayloadContainmentRow.
type HeldBinRow = protocol.HeldBinRow
