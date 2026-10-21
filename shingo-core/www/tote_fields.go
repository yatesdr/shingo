package www

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// tote_fields.go — how the admin doors read the tote numbers (payloads'
// tote_layers and uop_per_tote, bin_types.totes_per_layer, bins.totes_on_cart).
//
// Each is a nullable count: NULL is unknown, and a value is a real count. A
// door must tell three things apart, because a save that does not carry the
// field has to leave the stored value standing: the field absent (leave it),
// the field blank or null (unknown), and a number. Pin:
// TestPayloadDoors_UpdateRoundTrip.

// optionalCount is a JSON count that knows whether its key was sent. Set is
// false when the key was absent; Value is nil when it was null.
type optionalCount struct {
	Set   bool
	Value *int
}

// UnmarshalJSON is called for a present key only, null included.
func (c *optionalCount) UnmarshalJSON(b []byte) error {
	c.Set = true
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		c.Value = nil
		return nil
	}
	var v int
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	c.Value = &v
	return nil
}

// countAtLeast refuses a count below min, naming the field. nil passes: it is
// unknown, which every tote number may be.
func countAtLeast(name string, v *int, min int) error {
	if v != nil && *v < min {
		return fmt.Errorf("%s must be %d or more, or blank for unknown (got %d)", name, min, *v)
	}
	return nil
}

// formCount reads a count box from a parsed form. set is false when the form
// did not carry the field at all; a blank box is nil (unknown); anything else
// must be a whole number of at least min.
func formCount(r *http.Request, name string, min int) (value *int, set bool, err error) {
	raw, ok := r.Form[name]
	if !ok {
		return nil, false, nil
	}
	s := ""
	if len(raw) > 0 {
		s = strings.TrimSpace(raw[0])
	}
	if s == "" {
		return nil, true, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return nil, true, fmt.Errorf("%s %q is not a whole number", name, s)
	}
	if err := countAtLeast(name, &v, min); err != nil {
		return nil, true, err
	}
	return &v, true, nil
}
