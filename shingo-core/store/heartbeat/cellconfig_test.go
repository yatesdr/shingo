package heartbeat

import (
	"reflect"
	"testing"
)

// TestSubProcessIDsRoundTrip pins the JSONB encoding of sub_process_ids,
// including the empty/null edges (a JSONB column can hold the literal `null`
// and a nil Go slice must serialize as `[]`, never `null`, so the API and the
// decode stay stable).
func TestSubProcessIDsRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   []int64
	}{
		{"nil", nil},
		{"empty", []int64{}},
		{"one", []int64{42}},
		{"many", []int64{1, 2, 3}},
	}
	for _, c := range cases {
		b, err := marshalIDs(c.in)
		if err != nil {
			t.Fatalf("%s: marshal: %v", c.name, err)
		}
		got, err := unmarshalIDs(b)
		if err != nil {
			t.Fatalf("%s: unmarshal: %v", c.name, err)
		}
		want := c.in
		if want == nil {
			want = []int64{}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: round-trip = %v, want %v", c.name, got, want)
		}
	}
}

// TestUnmarshalIDsLenientInputs pins decode of the JSONB shapes the DB can hand
// back: an empty string (NULL coalesced) and the literal `null` both decode to
// an empty slice, not nil and not an error.
func TestUnmarshalIDsLenientInputs(t *testing.T) {
	for _, in := range []string{"", "null", "[]"} {
		got, err := unmarshalIDs([]byte(in))
		if err != nil {
			t.Errorf("unmarshalIDs(%q) errored: %v", in, err)
			continue
		}
		if got == nil || len(got) != 0 {
			t.Errorf("unmarshalIDs(%q) = %v, want empty slice", in, got)
		}
	}
}

// TestAllProcessIDs pins the primary+subs flattening used by the state query.
func TestAllProcessIDs(t *testing.T) {
	c := CellConfig{PrimaryProcessID: 10, SubProcessIDs: []int64{20, 30}}
	got := c.AllProcessIDs()
	want := []int64{10, 20, 30}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AllProcessIDs = %v, want %v", got, want)
	}
}
