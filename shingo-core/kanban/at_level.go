package kanban

import (
	"errors"
	"fmt"

	"shingo/protocol"
	"shingocore/domain"
)

// ErrAtLevel is the refusal a store into a kanban gets when the kanban already holds
// its level: the group has room, and the level says it must not take another carrier.
// Match it with errors.Is.
//
// TYPED, BECAUSE THE SENTENCE IS SHARED. A group with no free slot at all says the
// same words, and the words cannot change: classifyResolutionError and the park/queue
// paths read "no available slot in node group" as CAPACITY. A caller that must tell
// "at its level" from "full" (the cancel-return policy follows a kanban's overflow on
// the first and only the first) asks the type.
var ErrAtLevel = errors.New("kanban at its level")

// atLevelError carries ErrAtLevel with the capacity sentence every existing reader
// matches on, byte for byte.
type atLevelError struct{ group string }

func (e *atLevelError) Error() string        { return "no available slot in node group " + e.group }
func (e *atLevelError) Is(target error) bool { return target == ErrAtLevel }

// RefusedAtLevel is the refusal for one group: ErrAtLevel in the capacity sentence.
func RefusedAtLevel(group string) error { return &atLevelError{group: group} }

// AtLevel is the level question with the counts supplied: is a group whose declared
// levels are levels at its level for the cart stated, when count answers how many
// empties of a type it holds at a height and row the want of a type's tops & bottoms
// row (false when it has none). typeID and state say what the cart is: nil for a type
// that could not be named, blank state for a type named with no cart. Pure, so the
// capacity gate and the resolver's store ask this function and not a copy of it. The
// resolver passes the group as it stands, or as it will stand once a committed lift
// has emptied one child.
//
// A KNOWN TYPE IS JUDGED ON ITS OWN LINE, an unknown one on the group total: an
// untyped push has done nothing to deserve one satisfied type fencing every other.
//
// TWO LINES PER TYPE, AND NEITHER TAKES THE OTHER'S ROOM. A cart stated at 0 with no
// part (tops & bottoms) is judged on the type's tops & bottoms line: the carts at 0
// of the type in the group (count at TopsAndBottomsOnly) against the row's want. A
// type the group keeps with no such row refuses it; a type the group declares no level
// of does not, as on the stack line. Every other cart, and a type named with no cart,
// is judged on the stack line: the empties of the type at no height said, which leaves
// tops & bottoms out (domain.Height.Admits), against the level. The row is read only
// for a cart at 0. Pin: TestAtLevel_TwoLinesPerType.
func AtLevel(levels []TypeLevel, typeID *int64, state protocol.CarrierState,
	count func(binTypeCode string, h domain.Height) (int, error),
	row func(binTypeID int64) (want int, ok bool, err error)) (bool, error) {
	if len(levels) == 0 {
		return false, nil
	}

	if typeID != nil {
		for _, l := range levels {
			if l.BinTypeID != *typeID {
				continue
			}
			want, h := l.Want, domain.Height{}
			if state == protocol.CarrierTopsAndBottoms {
				rowWant, ok, rerr := row(l.BinTypeID)
				if rerr != nil {
					return false, fmt.Errorf("tops & bottoms row of %s: %w", l.BinTypeCode, rerr)
				}
				if !ok {
					return true, nil
				}
				want, h = rowWant, protocol.TopsAndBottomsOnly
			}
			held, cerr := count(l.BinTypeCode, h)
			if cerr != nil {
				return false, fmt.Errorf("count %s: %w", l.BinTypeCode, cerr)
			}
			return held >= want, nil
		}
		// A type nobody declared. NOT refused: a kanban is a group with a level on some
		// types, not a group closed to every other. Declaring a level is saying "hold at
		// least these"; it is not saying "and nothing else may ever stand here".
		return false, nil
	}

	want, held := 0, 0
	for _, l := range levels {
		want += l.Want
		n, cerr := count(l.BinTypeCode, domain.Height{})
		if cerr != nil {
			return false, fmt.Errorf("count %s: %w", l.BinTypeCode, cerr)
		}
		held += n
	}
	return held >= want, nil
}
