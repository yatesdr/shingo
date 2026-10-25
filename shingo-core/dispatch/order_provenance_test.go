package dispatch

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"shingo/protocol"
	"shingocore/store/orders"
)

// TestStage3_IsCoordinated pins the provenance discriminator: it reads the
// order.Coordinated COLUMN, NOT StepsJSON. The decoupling is the whole point of
// the provenance column — the unified-create follow-up persists a step plan onto
// a PLAIN order, and that order must still classify plain. A no-wait coordinated
// leg (changeover release) classifies coordinated because it was STAMPED so at
// intake, not because of its plan shape.
func TestStage3_IsCoordinated(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		coordinated bool
		steps       string
		want        bool
	}{
		{"plain single-transport order, unstamped", false, "", false},
		{"coordinated order (stamped at complex intake)", true, `[{"action":"pickup","node":"LINE"},{"action":"dropoff","node":"STORE"}]`, true},
		{"decoupling: a PLAIN order that persisted a step plan is still plain", false, `[{"action":"pickup","node":"LINE"},{"action":"dropoff","node":"STORE"}]`, false},
		{"coordinated stamp holds even with no persisted steps (defensive)", true, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			o := &orders.Order{Coordinated: c.coordinated, StepsJSON: c.steps}
			if got := IsCoordinated(o); got != c.want {
				t.Errorf("IsCoordinated(Coordinated=%v, StepsJSON=%q) = %v, want %v", c.coordinated, c.steps, got, c.want)
			}
		})
	}
}

// TestStage4_SourceIntentForType pins the label→data mapping used at intake.
//
// The "store" case is gone with the constant: it was never a kind of order, only
// the direction argument the bin resolver takes, and it now has its own type.
// What it used to guard here — that an unlisted type falls to the default rather
// than to "local" — is covered by the unknown-type case below, which is the same
// guard without implying that "store" is an order anyone can place.
func TestStage4_SourceIntentForType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		typ  protocol.OrderType
		want string
	}{
		{OrderTypeRetrieve, SourceIntentFull},       // full payload-matched bin
		{OrderTypeRetrieveEmpty, SourceIntentEmpty}, // generic empty carrier, at any stated height
		{OrderTypeMove, SourceIntentLocal},          // bin AT a concrete node
		// An unrecognised type must fall to full, NOT local: local exempts an
		// order from the scanner's payload guard, so a stray mapping there is
		// the expensive direction to be wrong in.
		{protocol.OrderType("something-nobody-has-added-yet"), SourceIntentFull},
		{OrderTypeComplex, SourceIntentFull}, // coordinated — sourced per-leg, not here
	}
	for _, c := range cases {
		if got := SourceIntentForType(c.typ); got != c.want {
			t.Errorf("SourceIntentForType(%s) = %q, want %q", c.typ, got, c.want)
		}
	}
}

// TestPlainFamily_EveryTypeHasItsIntentAndItsPlanner: the plain family is one
// list, and each member has its own arm in SourceIntentForType (no default
// sends a new member to the full finder), is planned by planTransport and is
// decided ahead of its row. A type added to plainFamily without an entry here
// fails the count.
func TestPlainFamily_EveryTypeHasItsIntentAndItsPlanner(t *testing.T) {
	t.Parallel()
	want := map[protocol.OrderType]string{
		OrderTypeRetrieve:      SourceIntentFull,
		OrderTypeRetrieveEmpty: SourceIntentEmpty,
		OrderTypeMove:          SourceIntentLocal,
	}
	if len(plainFamily) != len(want) {
		t.Fatalf("plainFamily has %d types, this pin names %d: give the new type its intent here and in SourceIntentForType",
			len(plainFamily), len(want))
	}
	s := newPlanningService(nil, nil, nil, nil, nil, nil, nil)
	for _, typ := range plainFamily {
		intent, ok := want[typ]
		if !ok {
			t.Errorf("%s is plain and this pin does not name its intent", typ)
			continue
		}
		if got := SourceIntentForType(typ); got != intent {
			t.Errorf("SourceIntentForType(%s) = %q, want %q", typ, got, intent)
		}
		if !s.Handles(typ) || !s.decidesAhead(typ) {
			t.Errorf("%s: planned %v, decided ahead %v; want both", typ, s.Handles(typ), s.decidesAhead(typ))
		}
	}
}

// TestRule4_NoOrderTypeOrIntentCarriesAHeight is ground rule 4
// (docs/ground-rules.md): a want is a Height on the demand, stated on the wire
// (protocol.OrderRequest.Want) and stored on the order, so no order type and no
// sourcing intent names a height or a carrier state. The tops & bottoms pull's
// type and intent are gone from every module (forbidigo's tombstone in
// .golangci.yml keeps them gone), and the plain family is three types.
func TestRule4_NoOrderTypeOrIntentCarriesAHeight(t *testing.T) {
	t.Parallel()
	var found []string
	root := repoRootFor(t)
	for _, module := range []string{"protocol", "shared", "shingo-core", "shingo-edge", "integration"} {
		err := filepath.WalkDir(filepath.Join(root, module), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return err
			}
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, p, nil, 0)
			if perr != nil {
				return perr
			}
			rel, rerr := filepath.Rel(root, p)
			if rerr != nil {
				return rerr
			}
			ast.Inspect(f, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok &&
					(id.Name == "OrderTypeRetrieveTopsAndBottoms" || id.Name == "SourceIntentTopsAndBottoms") {
					found = append(found, fmt.Sprintf("%s:%d: %s", filepath.ToSlash(rel), fset.Position(id.Pos()).Line, id.Name))
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", module, err)
		}
	}
	if len(plainFamily) != 3 {
		found = append(found, fmt.Sprintf("the plain family has %d members, not 3", len(plainFamily)))
	}
	for _, f := range found {
		t.Errorf("rule 4: a type or intent that carries a height: %s", f)
	}
}

// TestSourcesEmpty: the one test of an empty carrier's intent, whatever height
// its want states; a full, a move and a carried bin are not one, and neither is
// the retired tops & bottoms intent's word.
func TestSourcesEmpty(t *testing.T) {
	t.Parallel()
	for intent, want := range map[string]bool{
		SourceIntentEmpty: true, "tops_bottoms": false,
		SourceIntentFull: false, SourceIntentLocal: false, SourceIntentOnDeck: false,
	} {
		if got := SourcesEmpty(intent); got != want {
			t.Errorf("SourcesEmpty(%q) = %v, want %v", intent, got, want)
		}
	}
}

// TestCarrierPullIntent_ACoreAskIsAnEmptyOrAFull: a Core-authored carrier pull
// is an empty or a full; a move or a complex order is no Core ask's type.
func TestCarrierPullIntent_ACoreAskIsAnEmptyOrAFull(t *testing.T) {
	t.Parallel()
	for _, typ := range []protocol.OrderType{OrderTypeMove, OrderTypeComplex} {
		if _, ok := carrierPullIntent(typ); ok {
			t.Errorf("carrierPullIntent accepted %s; a Core ask is only an empty or a full", typ)
		}
	}
	for _, typ := range []protocol.OrderType{OrderTypeRetrieve, OrderTypeRetrieveEmpty} {
		if _, ok := carrierPullIntent(typ); !ok {
			t.Errorf("carrierPullIntent refused %s", typ)
		}
	}
}

// TestStage3_AssertSimpleNotCoordinated_DoesNotPanic exercises the tripwire on both a
// clean simple order and a (bug-state) simple order stamped coordinated — it must
// log loudly without panicking, and must ignore complex-type orders. Post-column
// the bug state is Coordinated=true on a plain type (NOT StepsJSON — the
// unified-create follow-up gives plain orders steps legitimately).
func TestStage3_AssertSimpleNotCoordinated_DoesNotPanic(t *testing.T) {
	t.Parallel()
	AssertSimpleNotCoordinated(&orders.Order{OrderType: OrderTypeRetrieve})                       // clean simple
	AssertSimpleNotCoordinated(&orders.Order{ID: 7, OrderType: OrderTypeMove, Coordinated: true}) // bug state — logs
	AssertSimpleNotCoordinated(&orders.Order{OrderType: OrderTypeMove, StepsJSON: `[{"a":1}]`})   // plain w/ steps — clean
	AssertSimpleNotCoordinated(&orders.Order{OrderType: OrderTypeComplex, Coordinated: true})     // complex — ignored
}

// TestClassifyInboundOrigin is the intake classification matrix. Every row is a
// message an Edge can actually send, including the ones it should not.
//
// The two that carry the design: an UNSTATED origin lands `orphan`, not
// `no_demand`, because Edge ships before Core and the skew window is precisely
// when every threshold order arrives bare — silencing it there would hide real
// losses for the length of a rollout. And a MALFORMED id lands orphan rather
// than failing the order: origin_id is a UUID column, so storing "not-a-uuid"
// would abort the INSERT and lose the transport work over a telemetry field.
func TestClassifyInboundOrigin(t *testing.T) {
	t.Parallel()
	const good = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
	cases := []struct {
		name      string
		id, class string
		wantID    string
		wantClass string
	}{
		{"attached: id and class agree", good, protocol.OriginClassAttached, good, protocol.OriginClassAttached},
		{"attached: id wins over a blank class", good, "", good, protocol.OriginClassAttached},
		{"attached: id wins over a contradicting class", good, protocol.OriginClassNoDemand, good, protocol.OriginClassAttached},
		{"no_demand: stamped by Edge at its create site", "", protocol.OriginClassNoDemand, "", protocol.OriginClassNoDemand},
		{"orphan: said nothing at all (an Edge that predates origins)", "", "", "", protocol.OriginClassOrphan},
		{"orphan: claimed attached with nothing to attach to", "", protocol.OriginClassAttached, "", protocol.OriginClassOrphan},
		{"orphan: said orphan outright", "", protocol.OriginClassOrphan, "", protocol.OriginClassOrphan},
		{"orphan: class off the enum", "", "sort-of-attached", "", protocol.OriginClassOrphan},
		{"orphan: id that is not a UUID (must not fail the order)", "not-a-uuid", protocol.OriginClassAttached, "", protocol.OriginClassOrphan},
		{"orphan: id that is nearly a UUID", good + "x", protocol.OriginClassAttached, "", protocol.OriginClassOrphan},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotID, gotClass := classifyInboundOrigin(c.id, c.class, "line-1", "uuid-test")
			if gotID != c.wantID {
				t.Errorf("origin_id = %q, want %q", gotID, c.wantID)
			}
			if gotClass != c.wantClass {
				t.Errorf("origin_class = %q, want %q", gotClass, c.wantClass)
			}
		})
	}
}

// TestClassifyInboundOrigin_NeverReturnsAnEmptyClass is the property the enum
// exists for. An empty origin_class is the state the column was added to
// abolish: "origin_id IS NULL AND the class is empty" is the old unanswerable
// question wearing a new column, and a row in it belongs to no bucket on the
// surface — invisible rather than merely uninteresting.
func TestClassifyInboundOrigin_NeverReturnsAnEmptyClass(t *testing.T) {
	t.Parallel()
	ids := []string{"", "6ba7b810-9dad-11d1-80b4-00c04fd430c8", "garbage", "  "}
	classes := []string{"", protocol.OriginClassAttached, protocol.OriginClassNoDemand, protocol.OriginClassOrphan, "junk"}
	for _, id := range ids {
		for _, cl := range classes {
			gotID, gotClass := classifyInboundOrigin(id, cl, "line-1", "uuid-test")
			if gotClass == "" {
				t.Errorf("classifyInboundOrigin(%q, %q) returned an EMPTY class — that row lands in no bucket", id, cl)
			}
			if gotID != "" && gotClass != protocol.OriginClassAttached {
				t.Errorf("classifyInboundOrigin(%q, %q) = (%q, %q): an id may only ever come back attached", id, cl, gotID, gotClass)
			}
		}
	}
}
