//go:build docker

package engine

import (
	"encoding/json"
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/messaging"
	"shingocore/service"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
)

// The reference case, end to end through the bins feed: the group at 0 of 3
// totes and 2 of 3 KD; a full tote cart, a full KD cart and an empty KD cart at
// the unloader's three windows. The Edge watching the windows publishes its
// list and the snapshot it is sent carries the group's line: the empty KD goes
// back as it is, both fulls are asked for totes. Rendered twice; the snapshot
// is read back off the outbox as the Edge would receive it.
func TestBinsFeed_ReferenceCaseLineInTheSnapshot(t *testing.T) {
	t.Parallel()
	f := newLinesFixture(t, "LINES-FEED")
	f.empty(t, f.kd, 0, "LINES-FEED-KD1", nil)
	f.empty(t, f.kd, 1, "LINES-FEED-KD2", nil)
	f.levels(t, map[int64]int{f.kd: 3, f.tote: 3}, nil)

	lid, err := f.db.CreateLoader(loaders.Loader{Name: "UL-FEED", Role: loaders.RoleConsume,
		Layout: loaders.LayoutSharedWindow, Replenishment: loaders.ReplenishmentOperator, OutboundDest: "LINES-FEED"})
	testutil.MustNoErr(t, err, "loader")
	carts := map[string]int64{}
	for i, w := range []struct {
		name string
		bt   int64
		full bool
	}{{"W1", f.tote, true}, {"W2", f.kd, true}, {"W3", f.kd, false}} {
		n := &nodes.Node{Name: w.name, Enabled: true}
		testutil.MustNoErr(t, f.db.CreateNode(n), "window "+w.name)
		testutil.MustNoErr(t, f.db.UpsertLoaderHome(loaders.Home{LoaderID: lid, PositionNodeID: n.ID, SortOrder: i}), "home")
		carts[w.name] = landAt(t, f.db, w.bt, n.ID, "CART-"+w.name)
		if w.full {
			testutil.MustNoErr(t, f.db.SetBinManifest(carts[w.name], `{"items": []}`, "PART-A", 10), "fill "+w.name)
		}
	}

	svc := messaging.NewCoreDataService(f.eng.db, messaging.NewCoreHandler(f.eng.db, nil, "core", "shingo.dispatch", nil),
		service.EpochAnnounce{})
	svc.SetGroupLinesFunc(f.eng.UnloaderGroupLines)
	names := []string{"W1", "W2", "W3"}
	digest, err := protocol.BinWatchDigest(names)
	testutil.MustNoErr(t, err, "watch digest")
	f.counter.Reset()
	svc.HandleBinWatch(&protocol.Envelope{Src: protocol.Address{Role: protocol.RoleEdge, Station: "edge.a"}},
		&protocol.BinWatchList{Digest: digest, Nodes: names})
	t.Logf("a watch list with one levelled group, received and answered: %d statements:\n%v",
		f.counter.Count(), f.counter.Statements())

	snap := outboxBinsSnapshot(t, f)
	first, err := json.Marshal(snap)
	testutil.MustNoErr(t, err, "marshal")
	second, err := json.Marshal(snap)
	testutil.MustNoErr(t, err, "marshal again")
	if string(first) != string(second) {
		t.Fatalf("two renderings differ:\n  %s\n  %s", first, second)
	}
	perLayer := map[string]string{"TOTE-L": `,"totes_per_layer":8`} // the type's, on every cart of it
	row := func(node string, full bool, bt string) string {
		if full {
			return fmt.Sprintf(`{"node":%q,"state":"occupied","occupied":true,"bins":1,"bin_id":%d,"bin_label":"CART-%s",`+
				`"bin_type_code":%q,"payload_code":"PART-A","manifest":"{\"items\": []}","manifest_confirmed":false,`+
				`"epoch":1,"stocked":true,"partial":false%s}`,
				node, carts[node], node, bt, perLayer[bt])
		}
		return fmt.Sprintf(`{"node":%q,"state":"occupied","occupied":true,"bins":1,"bin_id":%d,"bin_label":"CART-%s",`+
			`"bin_type_code":%q,"manifest_confirmed":false,"epoch":1,"stocked":false,"partial":false}`, node, carts[node], node, bt)
	}
	want := fmt.Sprintf(`{"digest":%q,"list_digest":%q,"rows":[%s,%s,%s],"groups":[{"group":"LINES-FEED",`+
		`"wants":[{"bin_type":"KD-L","want":3},{"bin_type":"TOTE-L","want":3}],`+
		`"assignments":[{"node":"W1","bin_type":"TOTE-L"},{"node":"W2","bin_type":"TOTE-L"},{"node":"W3","bin_type":"KD-L"}]}]}`,
		snap.Digest, digest, row("W1", true, "TOTE-L"), row("W2", true, "KD-L"), row("W3", false, "KD-L"))
	if string(first) != want {
		t.Errorf("snapshot =\n  %s\nwant\n  %s", first, want)
	}
	d, err := protocol.NodeBinsDigest(*snap)
	testutil.MustNoErr(t, err, "digest of the snapshot sent")
	if d != snap.Digest {
		t.Errorf("digest %s, NodeBinsDigest of the value sent %s", snap.Digest, d)
	}
	t.Logf("bytes per snapshot, three windows and one group line: %d raw", len(first))
}

// outboxBinsSnapshot reads the one bins snapshot the outbox holds.
func outboxBinsSnapshot(t *testing.T, f linesFixture) *protocol.NodeBinsSnapshot {
	t.Helper()
	var raw []byte
	testutil.MustNoErr(t, f.db.QueryRow(`SELECT payload FROM outbox WHERE msg_type = $1`,
		"data."+protocol.SubjectBinsSnapshot).Scan(&raw), "read the snapshot off the outbox")
	var env protocol.Envelope
	testutil.MustNoErr(t, json.Unmarshal(raw, &env), "envelope")
	var data protocol.Data
	testutil.MustNoErr(t, env.DecodePayload(&data), "data")
	var snap protocol.NodeBinsSnapshot
	testutil.MustNoErr(t, json.Unmarshal(data.Body, &snap), "snapshot")
	return &snap
}
