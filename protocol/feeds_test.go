package protocol

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
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
// case reads d(NodesDigest(..., nil, false)).
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
	base := d(NodesDigest(nodes, nil, bins, nil, false))
	if base != "66f5fcfce290e0b6" {
		t.Errorf("NodesDigest = %s, want 66f5fcfce290e0b6", base)
	}
	if got := d(NodesDigest(nodes, []LoaderInfo{}, bins, nil, false)); got != base {
		t.Errorf("nil vs empty loaders: %s != %s", got, base)
	}

	two := []NodeInfo{{Name: "LN-1", NodeType: "AP"}, {Name: "LN-2", NodeType: "AP"}}
	rev := []NodeInfo{two[1], two[0]}
	if a, b := d(NodesDigest(two, nil, nil, nil, false)), d(NodesDigest(rev, nil, nil, nil, false)); a != b {
		t.Errorf("reordered nodes: %s != %s", a, b)
	}
	loaders := []LoaderInfo{{LoaderKey: "loader:1"}, {LoaderKey: "loader:2"}}
	if a, b := d(NodesDigest(nil, loaders, nil, nil, false)),
		d(NodesDigest(nil, []LoaderInfo{loaders[1], loaders[0]}, nil, nil, false)); a != b {
		t.Errorf("reordered loaders: %s != %s", a, b)
	}

	for name, changed := range map[string]string{
		"node type":   d(NodesDigest([]NodeInfo{{Name: "LN-1", NodeType: "LM"}}, nil, bins, nil, false)),
		"kanban":      d(NodesDigest([]NodeInfo{{Name: "LN-1", NodeType: "AP", Kanban: true}}, nil, bins, nil, false)),
		"bin type":    d(NodesDigest(nodes, nil, []PayloadBinTypeInfo{{PayloadCode: "P1", BinTypeCode: "B2"}}, nil, false)),
		"loader":      d(NodesDigest(nodes, []LoaderInfo{{LoaderKey: "loader:1"}}, bins, nil, false)),
		"loader flag": d(NodesDigest(nodes, []LoaderInfo{{LoaderKey: "loader:1", AutoPush: true}}, bins, nil, false)),
		"floor":       d(NodesDigest(nodes, nil, bins, map[string]string{"B1": "45x48"}, false)),
		"plant-wide":  d(NodesDigest(nodes, nil, bins, nil, true)),
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

// TestCatalogDigest_ManifestLines: the catalog row carries its payload's
// manifest template lines. A row with none encodes as it did before the lines
// rode it (no key), so its digest is unchanged and a plant with no templates
// is not resent its catalog; a row with lines has its own golden, and a line's
// part, ratio, description and order each move it.
func TestCatalogDigest_ManifestLines(t *testing.T) {
	t.Parallel()
	d := digester(t)
	p1 := CatalogPayloadInfo{ID: 1, Name: "P1", Code: "P1", UOPCapacity: 10}
	empty := p1
	empty.Manifest = []ManifestItem{}
	if got := d(CatalogDigest([]CatalogPayloadInfo{empty})); got != "a3c26451e6bac0e0" {
		t.Errorf("a row with no lines digests %s, want the pre-lines a3c26451e6bac0e0", got)
	}
	withLines := p1
	withLines.Manifest = []ManifestItem{
		{PartNumber: "PN-1", PartsPerCycle: 2, Description: "left"},
		{PartNumber: "PN-2", PartsPerCycle: 1},
	}
	// [{"id":1,"name":"P1","code":"P1","description":"","uop_capacity":10,
	//   "manifest":[{"part_number":"PN-1","parts_per_cycle":2,"description":"left"},
	//               {"part_number":"PN-2","parts_per_cycle":1,"description":""}]}]
	base := d(CatalogDigest([]CatalogPayloadInfo{withLines}))
	if base != catalogLinesGolden {
		t.Errorf("CatalogDigest with lines = %s, want %s", base, catalogLinesGolden)
	}
	edit := func(f func(m []ManifestItem) []ManifestItem) string {
		c := withLines
		c.Manifest = f(append([]ManifestItem{}, withLines.Manifest...))
		return d(CatalogDigest([]CatalogPayloadInfo{c}))
	}
	for name, got := range map[string]string{
		"part":        edit(func(m []ManifestItem) []ManifestItem { m[0].PartNumber = "PN-9"; return m }),
		"ratio":       edit(func(m []ManifestItem) []ManifestItem { m[0].PartsPerCycle = 3; return m }),
		"description": edit(func(m []ManifestItem) []ManifestItem { m[1].Description = "right"; return m }),
		"order":       edit(func(m []ManifestItem) []ManifestItem { return []ManifestItem{m[1], m[0]} }),
		"dropped":     edit(func(m []ManifestItem) []ManifestItem { return m[:1] }),
	} {
		if got == base {
			t.Errorf("a %s change left the digest at %s", name, base)
		}
	}
}

const catalogLinesGolden = "4b3cad6e29957afc"

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

// TestCellCatalogDigest_Golden: the cells digest is fixed for a fixed catalog;
// the order of cells, nil against empty, and the order of one process's
// bindings (which the Edge's builder leaves unstable) do not move it; the order
// of processes (the first is the cell's primary at Core) and any binding field
// do.
func TestCellCatalogDigest_Golden(t *testing.T) {
	t.Parallel()
	d := digester(t)
	bind := func(proc, style int64, tag string) CellProcessBinding {
		return CellProcessBinding{ProcessID: proc, StyleID: style, PLCName: "PLC-1", TagName: tag}
	}
	cell := CellCatalogEntry{CellLabel: "PLC-1", Processes: []CellProcessBinding{bind(1, 10, "T1")}}
	// [{"cell_label":"PLC-1","processes":[{"process_id":1,"style_id":10,"plc_name":"PLC-1","tag_name":"T1"}]}]
	base := d(CellCatalogDigest([]CellCatalogEntry{cell}))
	if base != "2eb893ce87cd8586" {
		t.Errorf("CellCatalogDigest = %s, want 2eb893ce87cd8586", base)
	}
	if a, b := d(CellCatalogDigest(nil)), d(CellCatalogDigest([]CellCatalogEntry{})); a != b {
		t.Errorf("nil vs empty catalog: %s != %s", a, b)
	}
	if a, b := d(CellCatalogDigest([]CellCatalogEntry{{CellLabel: "PLC-2"}})),
		d(CellCatalogDigest([]CellCatalogEntry{{CellLabel: "PLC-2", Processes: []CellProcessBinding{}}})); a != b {
		t.Errorf("nil vs empty bindings: %s != %s", a, b)
	}
	other := CellCatalogEntry{CellLabel: "PLC-2", Processes: []CellProcessBinding{bind(2, 20, "T2")}}
	if a, b := d(CellCatalogDigest([]CellCatalogEntry{cell, other})),
		d(CellCatalogDigest([]CellCatalogEntry{other, cell})); a != b {
		t.Errorf("reordered cells: %s != %s", a, b)
	}
	oneProc := func(bs ...CellProcessBinding) string {
		return d(CellCatalogDigest([]CellCatalogEntry{{CellLabel: "PLC-1", Processes: bs}}))
	}
	if a, b := oneProc(bind(1, 10, "T1"), bind(1, 11, "T1"), bind(2, 10, "T2")),
		oneProc(bind(1, 11, "T1"), bind(1, 10, "T1"), bind(2, 10, "T2")); a != b {
		t.Errorf("one process's bindings reordered: %s != %s", a, b)
	}
	if a, b := oneProc(bind(1, 10, "T1"), bind(2, 10, "T2")), oneProc(bind(2, 10, "T2"), bind(1, 10, "T1")); a == b {
		t.Errorf("reordered processes (a new primary) left the digest at %s", a)
	}
	for name, changed := range map[string]string{
		"label":   d(CellCatalogDigest([]CellCatalogEntry{{CellLabel: "PLC-9", Processes: cell.Processes}})),
		"process": oneProc(bind(3, 10, "T1")),
		"style":   oneProc(bind(1, 12, "T1")),
		"tag":     oneProc(bind(1, 10, "T9")),
		"a cell":  d(CellCatalogDigest([]CellCatalogEntry{cell, other})),
	} {
		if changed == base {
			t.Errorf("%s change left the digest at %s", name, base)
		}
	}
}

// TestAllFeeds_NamesEveryFeedConst: AllFeeds lists every Feed* constant in
// feeds.go once, so a seventh feed declared without joining it fails here, and
// through AllFeeds each side's coverage test fails where its row is missing.
func TestAllFeeds_NamesEveryFeedConst(t *testing.T) {
	t.Parallel()
	constsListedOnce(t, "Feed", "AllFeeds", AllFeeds())
}

// TestAllOwned_NamesEveryOwnedConst: AllOwned lists every Owned* constant in
// feeds.go once, the same guard for the Edge-owned families.
func TestAllOwned_NamesEveryOwnedConst(t *testing.T) {
	t.Parallel()
	constsListedOnce(t, "Owned", "AllOwned", AllOwned())
}

// constsListedOnce fails unless list names every string constant in feeds.go
// whose name starts with prefix exactly once, and nothing else.
func constsListedOnce(t *testing.T, prefix, listName string, list []string) {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "feeds.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]int{}
	for _, k := range list {
		listed[k]++
	}
	consts := 0
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			for i, name := range spec.(*ast.ValueSpec).Names {
				lit, ok := spec.(*ast.ValueSpec).Values[i].(*ast.BasicLit)
				if !strings.HasPrefix(name.Name, prefix) || !ok {
					continue
				}
				consts++
				if v := strings.Trim(lit.Value, "\"`"); listed[v] != 1 {
					t.Errorf("%s = %s is listed %d times in %s, want once", name.Name, lit.Value, listed[v], listName)
				}
			}
		}
	}
	if consts != len(list) {
		t.Errorf("feeds.go declares %d %s* constants, %s lists %d", consts, prefix, listName, len(list))
	}
}

// TestOwnedDigest_Golden: a family's digest is fixed for fixed members; no
// members (nil or empty) is a real digest, not ""; a member's digest, a member
// added and a member held at "" each move it.
func TestOwnedDigest_Golden(t *testing.T) {
	t.Parallel()
	d := digester(t)
	// {"PR1":"f407896ad88a27dd"}
	base := d(OwnedDigest(map[string]string{"PR1": "f407896ad88a27dd"}))
	if base != "996a2883ccafde52" {
		t.Errorf("OwnedDigest = %s, want 996a2883ccafde52", base)
	}
	// {}
	none := d(OwnedDigest(nil))
	if none != "44136fa355b3678a" {
		t.Errorf("OwnedDigest(nil) = %s, want 44136fa355b3678a", none)
	}
	if got := d(OwnedDigest(map[string]string{})); got != none {
		t.Errorf("nil vs empty members: %s != %s", got, none)
	}
	for name, changed := range map[string]string{
		"a member's digest": d(OwnedDigest(map[string]string{"PR1": "0000000000000000"})),
		"a member added":    d(OwnedDigest(map[string]string{"PR1": "f407896ad88a27dd", "PR2": "f407896ad88a27dd"})),
		"a member at \"\"":  d(OwnedDigest(map[string]string{"PR1": "f407896ad88a27dd", "PR2": ""})),
		"no members":        none,
	} {
		if changed == base {
			t.Errorf("%s left the digest at %s", name, base)
		}
	}
}

// TestOwnedSingleton_AnEmptyValueIsNoMember: a family of one has its one
// member at the value's digest, and none for an empty value whatever digest
// the caller holds for it, so an Edge and a Core that both hold an empty
// value quote the same family digest as one that never held any.
func TestOwnedSingleton_AnEmptyValueIsNoMember(t *testing.T) {
	t.Parallel()
	d := digester(t)
	if got := OwnedSingleton("49a5c6a5e2c14030", false); len(got) != 1 || got[SingletonMember] != "49a5c6a5e2c14030" {
		t.Errorf("a held value = %v, want the one member %q at its digest", got, SingletonMember)
	}
	emptyList := d(BinWatchDigest(nil))
	got := OwnedSingleton(emptyList, true)
	if got == nil || len(got) != 0 {
		t.Errorf("an empty value = %#v, want no members", got)
	}
	if a, b := d(OwnedDigest(got)), d(OwnedDigest(nil)); a != b {
		t.Errorf("an empty value's family digest %s != a family never held %s", a, b)
	}
}

// TestOwned_AbsenceOnTheWire: the owned map's three absences stay apart in the
// bytes. nil is null on both messages (NOT omitempty: the peer does not speak
// it), {} is {} (speaks it, no family), and a family answered with no members
// leaves the key off (the digest compare carries the meaning).
func TestOwned_AbsenceOnTheWire(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		v    any
		want string
	}{
		{"a heartbeat from an Edge without the map", EdgeHeartbeat{Feeds: map[string]string{}},
			`{"feeds":{},"owned":null}`},
		{"a heartbeat naming no family", EdgeHeartbeat{Feeds: map[string]string{}, Owned: map[string]string{}},
			`{"feeds":{},"owned":{}}`},
		{"a heartbeat quoting a family", EdgeHeartbeat{Feeds: map[string]string{},
			Owned: map[string]string{OwnedCells: "0123456789abcdef"}},
			`{"feeds":{},"owned":{"cells":"0123456789abcdef"}}`},
		{"an ack with no owned answer", EdgeHeartbeatAck{StationID: "edge.a", Feeds: map[string]string{}},
			`{"station_id":"edge.a","server_ts":"0001-01-01T00:00:00Z","feeds":{},"owned":null}`},
		{"an answer that converged", OwnedAnswer{Digest: "0123456789abcdef"}, `{"digest":"0123456789abcdef"}`},
		{"an answer with members", OwnedAnswer{Digest: "0123456789abcdef", Members: map[string]string{"PR1": ""}},
			`{"digest":"0123456789abcdef","members":{"PR1":""}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := renderTwice(t, tc.v); got != tc.want {
				t.Errorf("rendered\n  %s\nwant\n  %s", got, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		body    string
		wantNil bool
	}{
		{`{"station_id":"edge.a","feeds":{}}`, true},
		{`{"feeds":{},"owned":null}`, true},
		{`{"feeds":{},"owned":{}}`, false},
	} {
		var hb EdgeHeartbeat
		if err := json.Unmarshal([]byte(tc.body), &hb); err != nil {
			t.Fatalf("decode %s: %v", tc.body, err)
		}
		if (hb.Owned == nil) != tc.wantNil {
			t.Errorf("%s decodes to Owned %#v, want nil %v", tc.body, hb.Owned, tc.wantNil)
		}
	}
}

// TestSourcingDigest_WhatADeltaWouldChange: the sourcing digest moves with
// what a delta carries (status, the lists, an at-risk line, the sentence) and
// not with ComputedAt or a line's time to empty; it does not depend on the
// order of the states, nor on nil against empty lists (the Edge's SQLite round
// trip stores [] for none).
func TestSourcingDigest_WhatADeltaWouldChange(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	base := []SourcingState{
		{ProcessID: "P1", StyleID: "A", Status: "green", Reason: "Can change over.", ComputedAt: at},
		{ProcessID: "P1", StyleID: "B", Status: "red", Missing: []string{"SYN-X"}, Absent: []string{"SYN-X"},
			Reason: "Cannot change over — missing SYN-X.", ComputedAt: at},
		{ProcessID: "P2", StyleID: "A", Status: "yellow", Reason: "Can change over, but SYN-Y running low — refill first.",
			AtRisk: []SourcingAtRisk{{PayloadCode: "SYN-Y", Node: "LINE-1", TimeToEmptySeconds: 600}}, ComputedAt: at},
	}
	clone := func() []SourcingState {
		out := make([]SourcingState, len(base))
		for i, s := range base {
			s.Missing = append([]string(nil), s.Missing...)
			s.Absent = append([]string(nil), s.Absent...)
			s.AtRisk = append([]SourcingAtRisk(nil), s.AtRisk...)
			out[i] = s
		}
		return out
	}
	digest := func(states []SourcingState) string {
		t.Helper()
		d, err := SourcingDigest(states)
		if err != nil {
			t.Fatalf("digest: %v", err)
		}
		return d
	}
	want := digest(base)

	same := map[string]func([]SourcingState){
		"ComputedAt moves":       func(s []SourcingState) { s[0].ComputedAt = at.Add(time.Hour) },
		"a time to empty drifts": func(s []SourcingState) { s[2].AtRisk[0].TimeToEmptySeconds = 480 },
		"the states reordered":   func(s []SourcingState) { s[0], s[2] = s[2], s[0] },
		"empty lists, not nil": func(s []SourcingState) {
			s[0].Missing, s[0].Absent, s[0].AtRisk = []string{}, []string{}, []SourcingAtRisk{}
		},
	}
	for name, mutate := range same {
		s := clone()
		mutate(s)
		if got := digest(s); got != want {
			t.Errorf("%s: digest moved (%s, want %s)", name, got, want)
		}
	}
	moved := map[string]func([]SourcingState){
		"a status":         func(s []SourcingState) { s[0].Status = "red" },
		"the missing list": func(s []SourcingState) { s[1].Missing = []string{"SYN-X", "SYN-Z"} },
		"the absent list":  func(s []SourcingState) { s[1].Absent = nil },
		"an at-risk line":  func(s []SourcingState) { s[2].AtRisk[0].Node = "LINE-2" },
		"the sentence":     func(s []SourcingState) { s[1].Reason += " 2 bins of SYN-X in undeclared carriers." },
		"a style dropped":  func(s []SourcingState) {},
	}
	for name, mutate := range moved {
		s := clone()
		mutate(s)
		if name == "a style dropped" {
			s = s[:2]
		}
		if got := digest(s); got == want {
			t.Errorf("%s changed and the digest did not", name)
		}
	}
}
