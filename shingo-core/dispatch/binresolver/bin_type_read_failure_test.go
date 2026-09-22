package binresolver

import (
	"errors"
	"testing"

	"shingocore/store/bins"
)

// bin_type_read_failure_test.go — the disposition of an unreadable
// node_bin_types list on the dispatch store-slot path.
//
// THE BUG THIS PINS was one condition doing two jobs:
//
//	if err != nil || len(bts) == 0 { return true }
//
// A list that could not be READ and a list that is EMPTY are different facts,
// and only one of them means "no restriction". Conflating them made a database
// fault open every node to every carrier for as long as the read kept failing —
// the direction that puts a bin somewhere it physically does not fit, and the
// one an operator has no way to see, because the refusal that should have
// happened simply did not.
//
// The two halves are asserted separately below so a future edit cannot restore
// the conflation and still pass: one test fixes the error and varies nothing
// else, the other fixes the empty list and varies nothing else.
//
// NOT IN SCOPE, and deliberately: engine.binTypeRefusal's bin_type_mode arm.
// That reader answers the stranded-transit inference path's question, carries
// the `specific`-with-none-assigned refusal on purpose, and its own comment
// records the decision that dispatch keeps its current reading. See
// binTypeAllowed's doc comment.

func ptrTo[T any](v T) *T { return &v }

// TestBinTypeAllowed_NilTypeAllows pins the guard that used to live at four call
// sites as `if binTypeID != nil`. Folding it into the predicate is only safe if
// nil still means "narrow nothing" — the reading every untyped resolve in the
// plant depends on.
func TestBinTypeAllowed_NilTypeAllows(t *testing.T) {
	t.Parallel()

	f := newFakeStore()
	f.effBinTypes[42] = []*bins.BinType{{ID: 7, Code: "FITS"}}
	r := &GroupResolver{DB: f}

	if !r.binTypeAllowed(42, nil) {
		t.Error("a nil carrier type was refused at a node that declares a set.\n\n" +
			"nil means the resolver could not work out what is being placed, not that " +
			"it is placing something unacceptable. Refusing here would close every " +
			"declared node to every order whose carrier could not be identified.")
	}
}

func TestBinTypeAllowed_ReadFailureRefuses(t *testing.T) {
	t.Parallel()

	f := newFakeStore()
	f.effBinTypesErr = errors.New("connection reset by peer")
	r := &GroupResolver{DB: f}

	if r.binTypeAllowed(42, ptrTo(int64(7))) {
		t.Error("an unreadable node_bin_types list allowed the carrier.\n\n" +
			"A list that could not be read is not an empty list. Allowing here opens " +
			"every node to every bin type for as long as the read keeps failing, and " +
			"nothing in the plant says so — the slot just takes a carrier that does " +
			"not fit. Refusing is recoverable: the resolver tries the next candidate.")
	}
}

// TestBinTypeAllowed_EmptyListAllows is the arm that must NOT move with it.
// node_bin_types is a restriction, not a whitelist that defaults closed — it is
// empty at Springfield, where 41 of 52 physical nodes declare nothing at all.
// Failing closed on an empty list would close every default slot in the plant.
func TestBinTypeAllowed_EmptyListAllows(t *testing.T) {
	t.Parallel()

	f := newFakeStore() // no error, no declarations
	r := &GroupResolver{DB: f}

	if !r.binTypeAllowed(42, ptrTo(int64(7))) {
		t.Error("a node with no declared bin types refused a carrier; node_bin_types " +
			"is a restriction, not a default-closed whitelist")
	}
}

// TestBinTypeAllowed_DeclaredSetIsRespected is the ordinary path, present so the
// two dispositions above are read against a working case rather than in
// isolation.
func TestBinTypeAllowed_DeclaredSetIsRespected(t *testing.T) {
	t.Parallel()

	f := newFakeStore()
	f.effBinTypes[42] = []*bins.BinType{{ID: 7, Code: "FITS"}}
	r := &GroupResolver{DB: f}

	if !r.binTypeAllowed(42, ptrTo(int64(7))) {
		t.Error("a declared bin type was refused at its own node")
	}
	if r.binTypeAllowed(42, ptrTo(int64(9))) {
		t.Error("an undeclared bin type was allowed at a node that declares a set")
	}
}
