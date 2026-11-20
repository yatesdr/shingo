package service

import (
	"errors"
	"strings"
	"testing"

	"shingo/protocol"
	"shingocore/store/inventory"
)

// Pure (no Postgres): the payload guards' decision (countUnitGuard). A ratio
// change on a payload whose bins or piles hold a count is a warning with each
// one's reading before and after, and no refusal; a change between one line
// and several while they do is refused, naming them; with nothing holding a
// count, neither.
func TestCountUnitGuard(t *testing.T) {
	t.Parallel()
	at := func(r int64) []protocol.ManifestItem {
		return []protocol.ManifestItem{{PartNumber: "P", PartsPerCycle: r}}
	}
	kit := []protocol.ManifestItem{{PartNumber: "A", PartsPerCycle: 1}, {PartNumber: "C", PartsPerCycle: 2}}
	holders := []inventory.Holder{{What: "BIN-1", Count: 800}, {What: "STN · ALN_1", Count: 24, Pile: true}}

	refuse, change := countUnitGuard("PANEL-A", at(8), at(4), holders, 1, 1)
	if refuse != nil || change == nil {
		t.Fatalf("8 to 4 with counts standing: refuse %v, change %v; want a warning", refuse, change)
	}
	if change.From != 8 || change.To != 4 || len(change.Rows) != 2 ||
		change.Rows[0] != (RatioChangeRow{What: "BIN-1", Parts: 800, UOPBefore: 100, UOPAfter: 200}) ||
		change.Rows[1] != (RatioChangeRow{What: "STN · ALN_1", Parts: 24, UOPBefore: 3, UOPAfter: 6, Pile: true}) {
		t.Errorf("the warning's rows = %+v, want BIN-1 800 parts 100 -> 200 UoP and the pile 24 parts 3 -> 6", change)
	}
	want := "Changing PANEL-A from 8 to 4 parts a cycle changes what 1 bin and 1 pile read. No count moves: " +
		"BIN-1 holds 800 parts: 100 UoP now, 200 UoP after; STN · ALN_1 holds 24 parts: 3 UoP now, 6 UoP after."
	if got := change.Sentence(); got != want {
		t.Errorf("the warning reads\n  %q\nwant\n  %q", got, want)
	}

	if refuse, change := countUnitGuard("PANEL-A", at(8), kit, holders, 1, 1); !errors.Is(refuse, ErrKitFlip) || change != nil ||
		!strings.Contains(refuse.Error(), "BIN-1") || !strings.Contains(refuse.Error(), "STN · ALN_1") {
		t.Errorf("one line to a kit with counts standing: refuse %v, change %v; want ErrKitFlip naming them", refuse, change)
	}
	if refuse, _ := countUnitGuard("KIT", kit, at(1), holders, 1, 1); !errors.Is(refuse, ErrKitFlip) {
		t.Errorf("a kit to one line with counts standing: %v, want ErrKitFlip", refuse)
	}
	if refuse, change := countUnitGuard("PANEL-A", at(8), at(4), nil, 0, 0); refuse != nil || change != nil {
		t.Errorf("8 to 4 with nothing holding a count: %v, %v; want nothing", refuse, change)
	}
	if refuse, change := countUnitGuard("PANEL-A", at(8), kit, nil, 0, 0); refuse != nil || change != nil {
		t.Errorf("one line to a kit with nothing holding a count: %v, %v; want nothing", refuse, change)
	}
	if refuse, change := countUnitGuard("PANEL-A", at(8), at(8), holders, 1, 1); refuse != nil || change != nil {
		t.Errorf("the same ratio: %v, %v; want nothing", refuse, change)
	}
	if refuse, change := countUnitGuard("NEW", nil, at(8), holders, 1, 1); refuse != nil || change == nil || change.From != 1 {
		t.Errorf("a payload with no line given one at 8: %v, %+v; want a warning from 1 to 8", refuse, change)
	}
}
