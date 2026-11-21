package heartbeat

import (
	"reflect"
	"testing"
)

// TestAllProcessIDs pins the primary+subs flattening used by the state query.
func TestAllProcessIDs(t *testing.T) {
	c := CellConfig{PrimaryProcessID: 10, SubProcessIDs: []int64{20, 30}}
	got := c.AllProcessIDs()
	want := []int64{10, 20, 30}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AllProcessIDs = %v, want %v", got, want)
	}
}
