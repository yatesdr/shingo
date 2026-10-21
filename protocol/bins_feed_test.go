package protocol

import (
	"testing"
)

// The node-bins feed's wire, byte for byte, and its two digests. Each value is
// rendered twice before its golden (renderTwice), and each digest is taken
// twice. A byte that moves here is a resend of every snapshot at every plant,
// so it moves only on purpose.

func TestNodeBinsSnapshot_Golden(t *testing.T) {
	t.Parallel()
	manifest := `{"items":[{"part_number":"PART-A"}]}`
	for _, tc := range []struct {
		name string
		v    any
		want string
	}{
		{
			name: "a row at a node with no bin",
			v:    NodeBinsRow{Node: "SMN_A1"},
			want: `{"node":"SMN_A1","occupied":false,"manifest_confirmed":false,"stocked":false}`,
		},
		{
			name: "a stocked tote cart",
			v: NodeBinsRow{
				Node: "SMN_A1", Occupied: true, BinID: 27, BinLabel: "TOTE-1", BinTypeCode: "TOTE",
				PayloadCode: "PART-A", Manifest: &manifest, ManifestConfirmed: true, Stocked: true,
				TotesOnCart: intPtr(16), TotesPerLayer: intPtr(8),
			},
			want: `{"node":"SMN_A1","occupied":true,"bin_id":27,"bin_label":"TOTE-1","bin_type_code":"TOTE",` +
				`"payload_code":"PART-A","manifest":"{\"items\":[{\"part_number\":\"PART-A\"}]}",` +
				`"manifest_confirmed":true,"stocked":true,"totes_on_cart":16,"totes_per_layer":8}`,
		},
		{
			name: "a base: a bare cart with no totes on it",
			v: NodeBinsRow{
				Node: "SMN_A2", Occupied: true, BinID: 28, BinLabel: "TOTE-2", BinTypeCode: "TOTE-BARE",
				Bare: true, TotesOnCart: intPtr(0),
			},
			want: `{"node":"SMN_A2","occupied":true,"bin_id":28,"bin_label":"TOTE-2","bin_type_code":"TOTE-BARE",` +
				`"bare":true,"manifest_confirmed":false,"stocked":false,"totes_on_cart":0}`,
		},
		{
			name: "a snapshot with no groups",
			v:    NodeBinsSnapshot{Digest: "0123456789abcdef", Rows: []NodeBinsRow{{Node: "SMN_A1"}}, Groups: []NodeBinsGroup{}},
			want: `{"digest":"0123456789abcdef","rows":[{"node":"SMN_A1","occupied":false,` +
				`"manifest_confirmed":false,"stocked":false}],"groups":[]}`,
		},
		{
			name: "a group's wants and assignments",
			v: NodeBinsGroup{
				Group: "KD-1",
				Wants: []NodeBinsWant{{BinType: "TOTE", Layers: intPtr(3), Want: 2}, {BinType: "KD", Want: 3}},
				Assignments: []NodeBinsAssignment{
					{Node: "SMN_A1", BinType: "TOTE", Layers: intPtr(3)}, {Node: "SMN_A2", BinType: "KD"},
				},
			},
			want: `{"group":"KD-1","wants":[{"bin_type":"TOTE","layers":3,"want":2},{"bin_type":"KD","want":3}],` +
				`"assignments":[{"node":"SMN_A1","bin_type":"TOTE","layers":3},{"node":"SMN_A2","bin_type":"KD"}]}`,
		},
		{
			name: "a watch list",
			v:    BinWatchList{Digest: "0123456789abcdef", Nodes: []string{"SMN_A1", "SMN_A2"}},
			want: `{"digest":"0123456789abcdef","nodes":["SMN_A1","SMN_A2"]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := renderTwice(t, tc.v); got != tc.want {
				t.Errorf("rendered\n  %s\nwant\n  %s", got, tc.want)
			}
		})
	}
}

// BinWatch on the ack: absent from a Core without the feed (nil), "" when Core
// holds no list for the Edge, the digest when it holds one. The rest of the
// ack's bytes are untouched by the field.
func TestEdgeHeartbeatAck_BinWatchGolden(t *testing.T) {
	t.Parallel()
	none, held := "", "0123456789abcdef"
	const head = `{"station_id":"edge.a","server_ts":"0001-01-01T00:00:00Z","feeds":{},"claims":{}`
	for _, tc := range []struct {
		name     string
		binWatch *string
		want     string
	}{
		{"a Core without the bins feed", nil, head + `}`},
		{"no list held", &none, head + `,"bin_watch":""}`},
		{"a list held", &held, head + `,"bin_watch":"0123456789abcdef"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ack := EdgeHeartbeatAck{StationID: "edge.a", Feeds: map[string]string{}, Claims: map[string]string{}, BinWatch: tc.binWatch}
			if got := renderTwice(t, ack); got != tc.want {
				t.Errorf("rendered\n  %s\nwant\n  %s", got, tc.want)
			}
		})
	}
}

// digestTwice takes a digest twice and fails when the two differ.
func digestTwice(t *testing.T, f func() (string, error)) string {
	t.Helper()
	d := digester(t)
	a, b := d(f()), d(f())
	if a != b {
		t.Fatalf("two digests of one value differ: %s, %s", a, b)
	}
	return a
}

func TestBinWatchDigest_Golden(t *testing.T) {
	t.Parallel()
	d := digester(t)
	// ["SMN_A1","SMN_A2"]
	base := digestTwice(t, func() (string, error) { return BinWatchDigest([]string{"SMN_A1", "SMN_A2"}) })
	if base != "49a5c6a5e2c14030" {
		t.Errorf("BinWatchDigest = %s, want 49a5c6a5e2c14030", base)
	}
	if got := d(BinWatchDigest([]string{"SMN_A2", "SMN_A1", "SMN_A2"})); got != base {
		t.Errorf("reordered and repeated names: %s != %s", got, base)
	}
	// []
	empty := d(BinWatchDigest(nil))
	if empty != "4f53cda18c2baa0c" {
		t.Errorf("BinWatchDigest(nil) = %s, want 4f53cda18c2baa0c", empty)
	}
	if got := d(BinWatchDigest([]string{})); got != empty {
		t.Errorf("nil vs empty: %s != %s", got, empty)
	}
	if got := d(BinWatchDigest([]string{"SMN_A1"})); got == base {
		t.Errorf("a name removed left the digest at %s", base)
	}
}

func TestNodeBinsDigest_Golden(t *testing.T) {
	t.Parallel()
	d := digester(t)
	row1 := NodeBinsRow{Node: "SMN_A1", Occupied: true, BinID: 27, BinLabel: "TOTE-1", BinTypeCode: "TOTE",
		PayloadCode: "PART-A", Stocked: true}
	row2 := NodeBinsRow{Node: "SMN_A2"}
	snap := NodeBinsSnapshot{Rows: []NodeBinsRow{row1, row2}}
	// {"rows":[{"node":"SMN_A1","occupied":true,"bin_id":27,"bin_label":"TOTE-1","bin_type_code":"TOTE",
	//   "payload_code":"PART-A","manifest_confirmed":false,"stocked":true},
	//  {"node":"SMN_A2","occupied":false,"manifest_confirmed":false,"stocked":false}],"groups":[]}
	base := digestTwice(t, func() (string, error) { return NodeBinsDigest(snap) })
	if base != "80fc38516b73a20c" {
		t.Errorf("NodeBinsDigest = %s, want 80fc38516b73a20c", base)
	}

	same := map[string]NodeBinsSnapshot{
		"its own Digest":  {Digest: base, Rows: snap.Rows},
		"reordered rows":  {Rows: []NodeBinsRow{row2, row1}},
		"empty groups":    {Rows: snap.Rows, Groups: []NodeBinsGroup{}},
		"rows not shared": {Rows: append([]NodeBinsRow{}, row1, row2)},
	}
	for name, s := range same {
		if got := d(NodeBinsDigest(s)); got != base {
			t.Errorf("%s moved the digest: %s != %s", name, got, base)
		}
	}

	g := func(wants []NodeBinsWant, assigned []NodeBinsAssignment) NodeBinsSnapshot {
		return NodeBinsSnapshot{Groups: []NodeBinsGroup{{Group: "KD-1", Wants: wants, Assignments: assigned}}}
	}
	wTote3, wKD := NodeBinsWant{BinType: "TOTE", Layers: intPtr(3), Want: 2}, NodeBinsWant{BinType: "KD", Want: 3}
	wTote := NodeBinsWant{BinType: "TOTE", Want: 1}
	a1, a2 := NodeBinsAssignment{Node: "SMN_A1", BinType: "TOTE"}, NodeBinsAssignment{Node: "SMN_A2", BinType: "KD"}
	if a, b := d(NodeBinsDigest(g([]NodeBinsWant{wTote3, wKD, wTote}, []NodeBinsAssignment{a1, a2}))),
		d(NodeBinsDigest(g([]NodeBinsWant{wTote, wKD, wTote3}, []NodeBinsAssignment{a2, a1}))); a != b {
		t.Errorf("reordered wants and assignments: %s != %s", a, b)
	}
	if a, b := d(NodeBinsDigest(g(nil, nil))), d(NodeBinsDigest(g([]NodeBinsWant{}, []NodeBinsAssignment{}))); a != b {
		t.Errorf("nil vs empty wants and assignments: %s != %s", a, b)
	}
	twoGroups := NodeBinsSnapshot{Groups: []NodeBinsGroup{{Group: "B"}, {Group: "A"}}}
	if a, b := d(NodeBinsDigest(twoGroups)),
		d(NodeBinsDigest(NodeBinsSnapshot{Groups: []NodeBinsGroup{{Group: "A"}, {Group: "B"}}})); a != b {
		t.Errorf("reordered groups: %s != %s", a, b)
	}

	changed := func(f func(*NodeBinsRow)) NodeBinsSnapshot {
		r := row1
		f(&r)
		return NodeBinsSnapshot{Rows: []NodeBinsRow{r, row2}}
	}
	manifest := `{"items":[]}`
	for name, s := range map[string]NodeBinsSnapshot{
		"stocked to not":      changed(func(r *NodeBinsRow) { r.Stocked = false }),
		"another bin":         changed(func(r *NodeBinsRow) { r.BinID = 28 }),
		"bin type":            changed(func(r *NodeBinsRow) { r.BinTypeCode = "KD" }),
		"bare":                changed(func(r *NodeBinsRow) { r.Bare = true }),
		"payload":             changed(func(r *NodeBinsRow) { r.PayloadCode = "PART-B" }),
		"manifest":            changed(func(r *NodeBinsRow) { r.Manifest = &manifest }),
		"manifest confirmed":  changed(func(r *NodeBinsRow) { r.ManifestConfirmed = true }),
		"totes on cart known": changed(func(r *NodeBinsRow) { r.TotesOnCart = intPtr(0) }),
		"totes per layer":     changed(func(r *NodeBinsRow) { r.TotesPerLayer = intPtr(8) }),
		"a group added":       {Rows: snap.Rows, Groups: []NodeBinsGroup{{Group: "KD-1"}}},
	} {
		if got := d(NodeBinsDigest(s)); got == base {
			t.Errorf("%s left the digest at %s", name, base)
		}
	}
	if a, b := d(NodeBinsDigest(g([]NodeBinsWant{wTote}, nil))),
		d(NodeBinsDigest(g([]NodeBinsWant{{BinType: "TOTE", Layers: intPtr(1), Want: 1}}, nil))); a == b {
		t.Errorf("a height on a want left the digest at %s", a)
	}
}
