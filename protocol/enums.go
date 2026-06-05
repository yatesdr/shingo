package protocol

import (
	"database/sql/driver"
	"fmt"
)

// ScanEnumNamed implements sql.Scanner for a string-enum type T, qualifying the
// error with typeName (e.g. "protocol.Status.Scan"). Accepts string, []byte, or
// nil (sets T to its zero value). No validation against a fixed set — historical
// rows from retired enum values must still load, so validation belongs at write
// time.
//
// This is the consolidation target for the package's string enums: each type's
// Scan delegates here with its own qualified name, so all six (protocol
// Status/SwapMode, domain BinStatus, edge NodeTaskState/ChangeoverState/
// StationTaskState) share one body while keeping their exact error text.
func ScanEnumNamed[T ~string](t *T, v any, typeName string) error {
	if v == nil {
		*t = ""
		return nil
	}
	switch x := v.(type) {
	case string:
		*t = T(x)
	case []byte:
		*t = T(x)
	default:
		return fmt.Errorf("%s: cannot scan %T", typeName, v)
	}
	return nil
}

// ScanEnum is the unqualified convenience form of ScanEnumNamed: it derives the
// error's type name from T (e.g. "protocol.MyEnum: cannot scan int"). Use it for
// a new enum where the generic message is good enough; use ScanEnumNamed when a
// test or log grep keys on an exact qualified message (e.g. a ".Scan" suffix).
//
//	func (s *MyEnum) Scan(v any) error {
//	    return ScanEnum(s, v)
//	}
func ScanEnum[T ~string](t *T, v any) error {
	return ScanEnumNamed(t, v, fmt.Sprintf("%T", *t))
}

// ValueEnum implements driver.Valuer for a string-enum type T.
func ValueEnum[T ~string](t T) (driver.Value, error) {
	return string(t), nil
}
