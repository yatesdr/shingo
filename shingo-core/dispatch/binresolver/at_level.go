package binresolver

import "errors"

// ErrAtDeclaredLevel is the refusal ResolveStore gives a maintained group that
// already holds its declared level (MG4-1): the group has room, and the level
// says it must not take another carrier. Match it with errors.Is.
//
// TYPED, BECAUSE THE SENTENCE IS SHARED. A group with no free slot at all says
// the same words, and the words cannot change: classifyResolutionError and the
// park/queue paths read "no available slot in node group" as CAPACITY. A
// caller that must tell "at its level" from "full" — the cancel-return policy
// follows a maintained group's overflow on the first and only the first —
// asks the type.
var ErrAtDeclaredLevel = errors.New("maintained group at its declared level")

// atLevelError carries ErrAtDeclaredLevel with the capacity sentence every
// existing reader matches on, byte for byte.
type atLevelError struct{ group string }

func (e *atLevelError) Error() string        { return "no available slot in node group " + e.group }
func (e *atLevelError) Is(target error) bool { return target == ErrAtDeclaredLevel }
