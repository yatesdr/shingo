package engine

import (
	"reflect"
	"testing"

	"shingocore/store/sourceability"
)

// D2.5 · the absent fact rides the existing feed: on the wire with the verdict,
// and a flip of it is a change the delta carries, because the Edge's dry-source
// guard decides on it.
func TestSourcingFeed_CarriesAbsentAndPublishesItsChange(t *testing.T) {
	t.Parallel()
	busy := sourceability.StyleState{ProcessID: "P", StyleID: "S", Status: sourceability.StatusRed, Missing: []string{"X"}}
	dry := busy
	dry.Absent = []string{"X"}

	if got := toWire(dry).Absent; !reflect.DeepEqual(got, []string{"X"}) {
		t.Errorf("wire absent = %v, want [X]", got)
	}
	if toWire(busy).Absent != nil {
		t.Errorf("a style with nothing absent sends %v", toWire(busy).Absent)
	}
	if !wireChanged(busy, dry) || !wireChanged(dry, busy) {
		t.Error("the last bin of X going (or coming back) is not a wire change; the Edge would hold the old answer until the snapshot")
	}
}
