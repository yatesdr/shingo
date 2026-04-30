package protocol

import (
	"database/sql/driver"
	"fmt"
)

// Status is the typed canonical order status. Wraps string so it serializes
// natively over JSON / SQL while gaining compile-time distinction from raw
// strings and other enum-shaped string types (e.g. bin status, ack outcome).
type Status string

// Canonical order status constants shared by core and edge.
const (
	StatusPending      Status = "pending"
	StatusSourcing     Status = "sourcing"
	StatusQueued       Status = "queued"
	StatusSubmitted    Status = "submitted"
	StatusDispatched   Status = "dispatched"
	StatusAcknowledged Status = "acknowledged"
	StatusInTransit    Status = "in_transit"
	StatusDelivered    Status = "delivered"
	StatusConfirmed    Status = "confirmed"
	StatusStaged       Status = "staged"
	StatusFailed       Status = "failed"
	StatusCancelled    Status = "cancelled"
	StatusReshuffling  Status = "reshuffling"
)

// IsTerminal reports whether the status has no outgoing transitions.
// Delegates to the package-level function to stay single-source-of-truth.
func (s Status) IsTerminal() bool {
	return IsTerminal(s)
}

// CanTransitionTo reports whether the (s, to) transition is allowed by the
// canonical state machine.
func (s Status) CanTransitionTo(to Status) bool {
	return IsValidTransition(s, to)
}

// String satisfies fmt.Stringer; convenient for log lines and debug output.
func (s Status) String() string {
	return string(s)
}

// Scan implements sql.Scanner for reading from a database column. Accepts
// string or []byte (both are common across drivers); NULL becomes the empty
// Status. Does not validate the value against AllStatuses() — historical rows
// from retired statuses must still load. Validation belongs at write time
// via DB CHECK constraints (deferred work) or at the LifecycleService.
func (s *Status) Scan(v any) error {
	if v == nil {
		*s = ""
		return nil
	}
	switch x := v.(type) {
	case string:
		*s = Status(x)
	case []byte:
		*s = Status(x)
	default:
		return fmt.Errorf("protocol.Status.Scan: cannot scan %T", v)
	}
	return nil
}

// Value implements driver.Valuer for writing to a database column.
func (s Status) Value() (driver.Value, error) {
	return string(s), nil
}

// AllStatuses returns every status defined in this module, used by
// table-driven tests that exhaustively cover the (from, to) matrix.
func AllStatuses() []Status {
	return []Status{
		StatusPending, StatusSourcing, StatusQueued, StatusSubmitted,
		StatusDispatched, StatusAcknowledged, StatusInTransit, StatusStaged,
		StatusDelivered, StatusConfirmed, StatusFailed, StatusCancelled,
		StatusReshuffling,
	}
}
