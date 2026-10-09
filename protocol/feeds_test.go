package protocol

import (
	"testing"
	"time"
)

// Golden digests, one per feed. A fixed value must give a fixed string, so a
// change to the encoding, the canonical copy or the hash is a visible break, not
// a silent resend of every feed at every plant. Then the three things that are
// not a change in the data — row order, time zone, nil versus empty — must not
// move the digest, and any field that is a change in the data must.
//
// The golden strings are SHA-256 of the JSON written beside each case.

// digester returns a func that unwraps a digest function's two results, so a
// case reads d(NodesDigest(...)).
func digester(t *testing.T) func(string, error) string {
	return func(d string, err error) string {
		t.Helper()
		if err != nil {
			t.Fatalf("digest: %v", err)
		}
		return d
	}
}

func TestDigest_Golden(t *testing.T) {
	t.Parallel()
	d := digester(t)
	// {"a":1}
	if got := d(Digest(map[string]int{"a": 1})); got != "015abd7f5cc57a2d" {
		t.Errorf("Digest = %s, want 015abd7f5cc57a2d", got)
	}
}

func TestNodesDigest_Golden(t *testing.T) {
	t.Parallel()
	d := digester(t)
	nodes := []NodeInfo{{Name: "LN-1", NodeType: "AP"}}
	bins := []PayloadBinTypeInfo{{PayloadCode: "P1", BinTypeCode: "B1"}}
	// {"nodes":[{"name":"LN-1","node_type":"AP"}],"loaders":[],"payload_bin_types":[{"payload_code":"P1","bin_type_code":"B1"}]}
	base := d(NodesDigest(nodes, nil, bins))
	if base != "66f5fcfce290e0b6" {
		t.Errorf("NodesDigest = %s, want 66f5fcfce290e0b6", base)
	}
	if got := d(NodesDigest(nodes, []LoaderInfo{}, bins)); got != base {
		t.Errorf("nil vs empty loaders: %s != %s", got, base)
	}

	two := []NodeInfo{{Name: "LN-1", NodeType: "AP"}, {Name: "LN-2", NodeType: "AP"}}
	rev := []NodeInfo{two[1], two[0]}
	if a, b := d(NodesDigest(two, nil, nil)), d(NodesDigest(rev, nil, nil)); a != b {
		t.Errorf("reordered nodes: %s != %s", a, b)
	}
	loaders := []LoaderInfo{{LoaderKey: "loader:1"}, {LoaderKey: "loader:2"}}
	if a, b := d(NodesDigest(nil, loaders, nil)),
		d(NodesDigest(nil, []LoaderInfo{loaders[1], loaders[0]}, nil)); a != b {
		t.Errorf("reordered loaders: %s != %s", a, b)
	}

	for name, changed := range map[string]string{
		"node type":   d(NodesDigest([]NodeInfo{{Name: "LN-1", NodeType: "LM"}}, nil, bins)),
		"maintained":  d(NodesDigest([]NodeInfo{{Name: "LN-1", NodeType: "AP", Maintained: true}}, nil, bins)),
		"bin type":    d(NodesDigest(nodes, nil, []PayloadBinTypeInfo{{PayloadCode: "P1", BinTypeCode: "B2"}})),
		"loader":      d(NodesDigest(nodes, []LoaderInfo{{LoaderKey: "loader:1"}}, bins)),
		"loader flag": d(NodesDigest(nodes, []LoaderInfo{{LoaderKey: "loader:1", AutoPush: true}}, bins)),
	} {
		if changed == base {
			t.Errorf("%s change left the digest at %s", name, base)
		}
	}
}

func TestCatalogDigest_Golden(t *testing.T) {
	t.Parallel()
	d := digester(t)
	p1 := CatalogPayloadInfo{ID: 1, Name: "P1", Code: "P1", UOPCapacity: 10}
	// [{"id":1,"name":"P1","code":"P1","description":"","uop_capacity":10}]
	base := d(CatalogDigest([]CatalogPayloadInfo{p1}))
	if base != "a3c26451e6bac0e0" {
		t.Errorf("CatalogDigest = %s, want a3c26451e6bac0e0", base)
	}
	p2 := CatalogPayloadInfo{ID: 2, Name: "P2", Code: "P2"}
	if a, b := d(CatalogDigest([]CatalogPayloadInfo{p1, p2})),
		d(CatalogDigest([]CatalogPayloadInfo{p2, p1})); a != b {
		t.Errorf("reordered payloads: %s != %s", a, b)
	}
	if a, b := d(CatalogDigest(nil)), d(CatalogDigest([]CatalogPayloadInfo{})); a != b {
		t.Errorf("nil vs empty: %s != %s", a, b)
	}
	changed := p1
	changed.CATID = "123"
	if got := d(CatalogDigest([]CatalogPayloadInfo{changed})); got == base {
		t.Errorf("CATID change left the digest at %s", base)
	}
}

func TestContainmentDigest_Golden(t *testing.T) {
	t.Parallel()
	d := digester(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	flag := PayloadContainmentRow{PayloadCode: "P1", Active: true, Reason: "r", ActivatedBy: "qa", ActivatedAt: &at}
	snap := ContainmentSnapshot{Flags: []PayloadContainmentRow{flag}}
	// {"flags":[{"payload_code":"P1","active":true,"reason":"r","activated_by":"qa",
	//   "activated_at":"2026-10-01T12:00:00Z","deactivated_by":"","deactivated_at":null}],
	//  "held_bins":[],"destinations":[]}
	base := d(ContainmentDigest(snap))
	if base != "495ee5733d2adacc" {
		t.Errorf("ContainmentDigest = %s, want 495ee5733d2adacc", base)
	}

	// The snapshot's own Digest is not part of it.
	sealed := snap
	sealed.Digest = base
	if got := d(ContainmentDigest(sealed)); got != base {
		t.Errorf("Digest field changed the digest: %s != %s", got, base)
	}
	// The same instant in another zone is the same data.
	local := at.In(time.FixedZone("CDT", -5*3600))
	zoned := ContainmentSnapshot{Flags: []PayloadContainmentRow{{PayloadCode: "P1", Active: true, Reason: "r", ActivatedBy: "qa", ActivatedAt: &local}}}
	if got := d(ContainmentDigest(zoned)); got != base {
		t.Errorf("non-UTC time: %s != %s", got, base)
	}
	empty := ContainmentSnapshot{Flags: snap.Flags, HeldBins: []HeldBinRow{}, Destinations: []ContainmentDestination{}}
	if got := d(ContainmentDigest(empty)); got != base {
		t.Errorf("nil vs empty: %s != %s", got, base)
	}

	dest := func(children []string, bins ...ContainmentBin) ContainmentSnapshot {
		return ContainmentSnapshot{Destinations: []ContainmentDestination{{Node: "QA-1", Children: children, Bins: bins}}}
	}
	b1 := ContainmentBin{Node: "QA-1.A", BinID: 1, Label: "B-1", PayloadCode: "P1", UOP: 5}
	b2 := ContainmentBin{Node: "QA-1.B", BinID: 2, Label: "B-2", PayloadCode: "P1", UOP: 5}
	if a, b := d(ContainmentDigest(dest([]string{"A", "B"}, b1, b2))),
		d(ContainmentDigest(dest([]string{"B", "A"}, b2, b1))); a != b {
		t.Errorf("reordered children and bins: %s != %s", a, b)
	}
	if a, b := d(ContainmentDigest(dest(nil))), d(ContainmentDigest(dest([]string{}))); a != b {
		t.Errorf("nil vs empty children: %s != %s", a, b)
	}

	moved := b1
	moved.UOP = 4
	if a, b := d(ContainmentDigest(dest(nil, b1))), d(ContainmentDigest(dest(nil, moved))); a == b {
		t.Errorf("a bin's count change left the digest at %s", a)
	}
	off := flag
	off.Active = false
	if got := d(ContainmentDigest(ContainmentSnapshot{Flags: []PayloadContainmentRow{off}})); got == base {
		t.Errorf("flag deactivation left the digest at %s", base)
	}
	held := ContainmentSnapshot{Flags: snap.Flags, HeldBins: []HeldBinRow{{BinID: 7, Label: "B-7", NodeName: "LN-1"}}}
	if got := d(ContainmentDigest(held)); got == base {
		t.Errorf("a held bin left the digest at %s", base)
	}
}

func TestRefusalsDigest_Golden(t *testing.T) {
	t.Parallel()
	d := digester(t)
	r1 := SupplyRefusalState{Action: SupplyRefusalOpened, LoaderNode: "LN-1", PayloadCode: "P1", RefusedBy: "edge.test",
		RefusedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	// [{"loader_node":"LN-1","payload_code":"P1","refused_by":"edge.test","ack_choice":"","ack_process_id":""}]
	base := d(RefusalsDigest([]SupplyRefusalState{r1}))
	if base != "d394edd9288f88df" {
		t.Errorf("RefusalsDigest = %s, want d394edd9288f88df", base)
	}
	// []
	if got := d(RefusalsDigest(nil)); got != "4f53cda18c2baa0c" {
		t.Errorf("RefusalsDigest(nil) = %s, want 4f53cda18c2baa0c", got)
	}
	if got := d(RefusalsDigest([]SupplyRefusalState{})); got != "4f53cda18c2baa0c" {
		t.Errorf("RefusalsDigest(empty) = %s, want 4f53cda18c2baa0c", got)
	}

	// Times and the action word are not part of it: each side stamps its own
	// refused_at, and Core's rows all read back as Opened.
	other := r1
	other.RefusedAt = r1.RefusedAt.Add(time.Hour)
	other.Action = SupplyRefusalAcked
	if got := d(RefusalsDigest([]SupplyRefusalState{other})); got != base {
		t.Errorf("time/action changed the digest: %s != %s", got, base)
	}

	r2 := SupplyRefusalState{LoaderNode: "LN-1", PayloadCode: "P2", RefusedBy: "edge.test"}
	r3 := SupplyRefusalState{LoaderNode: "LN-0", PayloadCode: "P9", RefusedBy: "edge.test"}
	if a, b := d(RefusalsDigest([]SupplyRefusalState{r1, r2, r3})),
		d(RefusalsDigest([]SupplyRefusalState{r3, r2, r1})); a != b {
		t.Errorf("reordered rows: %s != %s", a, b)
	}

	acked := r1
	acked.AckChoice, acked.AckProcessID = SupplyRefusalChoiceWait, "PR1"
	if got := d(RefusalsDigest([]SupplyRefusalState{acked})); got == base {
		t.Errorf("an ack left the digest at %s", base)
	}
	by := r1
	by.RefusedBy = "edge.other"
	if got := d(RefusalsDigest([]SupplyRefusalState{by})); got == base {
		t.Errorf("refused_by change left the digest at %s", base)
	}
}

func TestClaimsDigest_Golden(t *testing.T) {
	t.Parallel()
	d := digester(t)
	claim := PlantClaim{CoreNodeName: "LN-1", Role: ClaimRoleConsume, SwapMode: "simple", PayloadCode: "P1", UOPCapacity: 10}
	report := PlantClaimsReport{ProcessID: "PR1", Styles: []PlantClaimsStyle{{StyleID: "S1", Claims: []PlantClaim{claim}}}}
	// {"process_id":"PR1","styles":[{"style_id":"S1","claims":[{"core_node_name":"LN-1","role":"consume",
	//   "swap_mode":"simple","payload_code":"P1","allowed_payload_codes":[],"uop_capacity":10,"reorder_point":0}]}]}
	base := d(ClaimsDigest(report))
	if base != "f407896ad88a27dd" {
		t.Errorf("ClaimsDigest = %s, want f407896ad88a27dd", base)
	}

	sealed := report
	sealed.Digest = base
	if got := d(ClaimsDigest(sealed)); got != base {
		t.Errorf("Digest field changed the digest: %s != %s", got, base)
	}
	emptyAllowed := claim
	emptyAllowed.AllowedPayloadCodes = []string{}
	if got := d(ClaimsDigest(PlantClaimsReport{ProcessID: "PR1",
		Styles: []PlantClaimsStyle{{StyleID: "S1", Claims: []PlantClaim{emptyAllowed}}}})); got != base {
		t.Errorf("nil vs empty allowed payloads: %s != %s", got, base)
	}

	s1 := PlantClaimsStyle{StyleID: "S1", Claims: []PlantClaim{claim}}
	s2 := PlantClaimsStyle{StyleID: "S2", Active: true}
	if a, b := d(ClaimsDigest(PlantClaimsReport{ProcessID: "PR1", Styles: []PlantClaimsStyle{s1, s2}})),
		d(ClaimsDigest(PlantClaimsReport{ProcessID: "PR1", Styles: []PlantClaimsStyle{s2, s1}})); a != b {
		t.Errorf("reordered styles: %s != %s", a, b)
	}

	// The order of a style's claims is data: Core stores each claim's index.
	c2 := PlantClaim{CoreNodeName: "LN-2", Role: ClaimRoleConsume, PayloadCode: "P2"}
	if a, b := d(ClaimsDigest(PlantClaimsReport{ProcessID: "PR1", Styles: []PlantClaimsStyle{{StyleID: "S1", Claims: []PlantClaim{claim, c2}}}})),
		d(ClaimsDigest(PlantClaimsReport{ProcessID: "PR1", Styles: []PlantClaimsStyle{{StyleID: "S1", Claims: []PlantClaim{c2, claim}}}})); a == b {
		t.Errorf("reordered claims left the digest at %s", a)
	}
	active := report
	active.Styles = []PlantClaimsStyle{{StyleID: "S1", Claims: []PlantClaim{claim}, Active: true}}
	if got := d(ClaimsDigest(active)); got == base {
		t.Errorf("active style change left the digest at %s", base)
	}
}
