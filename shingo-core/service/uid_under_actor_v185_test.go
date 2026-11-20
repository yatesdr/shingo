package service

import (
	"maps"
	"slices"
	"testing"

	"shingocore/store"
)

// TestV185_MovesReadTheCensusLists holds migration 185's move lists equal to
// the step-0 census's (uid_under_actor_census_test.go): the census is what
// found which writers put a station uid in actor, and the migration moves
// exactly those rows. A writer the census learns of, or a list the migration
// edits by hand, fails here until the two agree.
func TestV185_MovesReadTheCensusLists(t *testing.T) {
	t.Parallel()
	if got, want := slices.Sorted(slices.Values(store.V185LedgerUIDOps)), slices.Sorted(slices.Values(UIDUnderActorLedgerOps)); !slices.Equal(got, want) {
		t.Errorf("v185 moves the ledger ops %v, the census names %v", got, want)
	}
	if got, want := slices.Sorted(maps.Keys(store.V185ExceptionUIDOps)), slices.Sorted(slices.Values(UIDUnderActorExceptionKinds)); !slices.Equal(got, want) {
		t.Errorf("v185 moves the exception kinds %v, the census names %v", got, want)
	}
	if !maps.EqualFunc(store.V185ExceptionUIDOps, UIDUnderActorExceptionOps, func(a, b []string) bool {
		return (a == nil) == (b == nil) && slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
	}) {
		t.Errorf("v185's (kind, op) set %v differs from the census's %v", store.V185ExceptionUIDOps, UIDUnderActorExceptionOps)
	}
	if !maps.Equal(store.V185ExceptionDetailUID, UIDInExceptionDetail) {
		t.Errorf("v185 moves the detail keys %v, the census names %v", store.V185ExceptionDetailUID, UIDInExceptionDetail)
	}
	if got, want := slices.Sorted(slices.Values(store.V185LedgerActorStationOps)), slices.Sorted(slices.Values(ActorInLedgerStationOps)); !slices.Equal(got, want) {
		t.Errorf("v185 moves a Core actor out of station for the ops %v, the census names %v", got, want)
	}
}
