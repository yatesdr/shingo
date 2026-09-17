package protocol

import (
	"testing"
)

// constants_test.go — the actor half of the UOPAdjustment contract.
//
// NOT a red-first guard — it characterises behaviour that already holds, and
// it is here because nothing held it. IsLifecycleActor is the discriminator
// Edge's bind repair turns on (shingo-edge/engine/handler_uop_adjustment.go):
// an adjustment naming a bin at a node with NO bin bound binds it when a
// person declared the number, and is ignored when Core's own bookkeeping
// announced it. Core picks which by the string it puts in one field. Neither
// side asserted the mapping, so the whole of that decision rested on two
// comparisons nobody had written a test around.

// TestIsLifecycleActor_ClassifiesEveryActorThatTravels pins which senders Edge
// reads as machine. The consequence of each row is in its `why`, because a
// wrong answer here is not a failed assertion in production — it is either a
// carrier that never binds and stops counting, or ticks charged to a carrier
// that has already been driven away.
func TestIsLifecycleActor_ClassifiesEveryActorThatTravels(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		actor     string
		lifecycle bool
		why       string
	}{
		{
			name: "unattributed", actor: "", lifecycle: true,
			why: "a producer that forgets to set an actor must get the safe answer: " +
				"leave the slot alone and let the arriving carrier bind itself on delivery",
		},
		{
			name: "core lifecycle", actor: ActorCoreLifecycle, lifecycle: true,
			why: "the generation announcement fires on produce-finalize too, and that one " +
				"routinely lands after a robot has lifted the carrier it names",
		},
		{
			name: "web ui", actor: AuditActorUI, lifecycle: false,
			why: "this is the value Core stamps on an admin door's declaration, and it is " +
				"what routes the message into the bind repair instead of the not-binding guard",
		},
		{
			name: "named operator", actor: "SNF3 Operator Screen", lifecycle: false,
			why: "the cycle-count door carries a free-form name, so humans cannot be " +
				"enumerated — anything that is not a known machine value is a person",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsLifecycleActor(tc.actor); got != tc.lifecycle {
				t.Errorf("IsLifecycleActor(%q) = %v, want %v — %s", tc.actor, got, tc.lifecycle, tc.why)
			}
		})
	}
}

// TestActorConstants_AreTheWireValues pins the two strings themselves.
//
// They are not internal names: Core writes them into a UOPAdjustment and an
// Edge running a different build compares against its own copy. A rename that
// compiles on both sides still changes what the wire says, and the failure is
// silent in exactly one direction — an Edge that stops recognising "core"
// starts binding empty slots from produce-finalize announcements, which is the
// misattribution the discriminator exists to prevent.
func TestActorConstants_AreTheWireValues(t *testing.T) {
	t.Parallel()
	if ActorCoreLifecycle != "core" {
		t.Errorf("ActorCoreLifecycle = %q, want \"core\" — an older Edge compares against the literal", ActorCoreLifecycle)
	}
	if AuditActorUI != "ui" {
		t.Errorf("AuditActorUI = %q, want \"ui\" — an older Edge compares against the literal", AuditActorUI)
	}
}

// TestDeclarer_RoundTripsThroughTheReader closes the loop between the two
// halves: what a sender chooses has to be what the reader concludes. Both
// sides are small enough to read and were still free to drift, because the
// only thing joining them was a string neither end asserted.
func TestDeclarer_RoundTripsThroughTheReader(t *testing.T) {
	t.Parallel()
	if got := DeclaredByPerson.Actor(); IsLifecycleActor(got) {
		t.Errorf("DeclaredByPerson.Actor() = %q, which IsLifecycleActor reads as machine — "+
			"the admin doors would announce a load the Edge then refuses to bind, which is the "+
			"unbound-and-not-counting state this distinction exists to end", got)
	}
	if got := DeclaredByLifecycle.Actor(); !IsLifecycleActor(got) {
		t.Errorf("DeclaredByLifecycle.Actor() = %q, which IsLifecycleActor reads as a person — "+
			"produce finalize would bind a carrier a robot has already driven away, and every "+
			"part the press makes afterwards would be charged to it", got)
	}
}

// TestDeclarer_ZeroValueIsTheSafeAnswer pins the floor. The parameter is
// required at every reset site, so this is not how the value is normally
// chosen — but a struct literal or a zero var that reaches the wire must fail
// in the direction that leaves the slot alone and waits for the arriving
// carrier to bind itself, which costs nothing.
func TestDeclarer_ZeroValueIsTheSafeAnswer(t *testing.T) {
	t.Parallel()
	var d Declarer
	if d != DeclaredByLifecycle {
		t.Fatalf("zero Declarer = %d, want DeclaredByLifecycle (%d)", d, DeclaredByLifecycle)
	}
	if !IsLifecycleActor(d.Actor()) {
		t.Errorf("zero Declarer announces %q, which binds an empty slot", d.Actor())
	}
}
