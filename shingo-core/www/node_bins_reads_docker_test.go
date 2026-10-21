//go:build docker

package www

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/config"
	"shingocore/engine"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
)

// TestNodeBins_TwoReadsWhateverTheNodeCount pins that the node-bins answer
// costs two statements however many nodes it is asked about: one that resolves
// every name (nodes.GetByDotNames) and one that reads every resolved node's
// bins (bins.ListByNodes). Edges poll this route for each station's nodes, so a
// read per name grows with the board.
//
// The names mix both arms of the resolver (dotted children of a group and bare
// names) and both answers a known node can give (a bin, no bin); the rows are
// checked as well as the count, so the two reads give what the per-node reads
// gave. The JSON itself is TestNodeBins_Golden's.
//
// Counted with store.OpenCounting on an engine that is built but not started,
// so nothing but the handler issues statements between Reset and Count.
func TestNodeBins_TwoReadsWhateverTheNodeCount(t *testing.T) {
	t.Parallel()
	db, dbCfg := testdb.OpenWithConfig(t)
	sd := testdb.SetupStandardData(t, db)

	grp := &nodes.Node{Name: "NBR-GRP", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(grp), "create the group")

	// 31 names: the first 10 are children asked for as GROUP.CHILD, the rest
	// bare. Every even-numbered node holds a bin, and every fourth a second,
	// newer one, which is the one the answer names (the first by id DESC).
	var names []string
	label := map[string]string{} // the bin each name reports; "" for none
	for i := 0; i < 31; i++ {
		n := &nodes.Node{Enabled: true}
		name := fmt.Sprintf("NBR-N%02d", i)
		if i < 10 {
			n.ParentID = &grp.ID
			name = fmt.Sprintf("NBR-C%02d", i)
		}
		n.Name = name
		testutil.MustNoErr(t, db.CreateNode(n), "create "+name)
		if i < 10 {
			name = grp.Name + "." + name
		}
		binsHere := 0
		switch {
		case i%4 == 0:
			binsHere = 2
		case i%2 == 0:
			binsHere = 1
		}
		for k := 0; k < binsHere; k++ {
			label[name] = fmt.Sprintf("NBR-BIN-%02d-%d", i, k)
			testdb.CreateBinAtNode(t, db, sd.Payload.Code, n.ID, label[name])
		}
		names = append(names, name)
	}

	cdb, counter, err := store.OpenCounting(dbCfg)
	testutil.MustNoErr(t, err, "open counting store")
	t.Cleanup(func() { cdb.Close() })
	eng := engine.New(engine.Config{AppConfig: config.Defaults(), DB: cdb, Fleet: simulator.New(), LogFunc: t.Logf})
	h := &Handlers{engine: eng, orchestration: eng}

	for _, n := range []int{22, 31} {
		asked := names[:n]
		counter.Reset()
		rec := getPlain(t, h.apiTelemetryNodeBins, "/api/telemetry/node-bins?nodes="+strings.Join(asked, ","))
		count, stmts := counter.Count(), counter.Statements()
		if rec.Code != http.StatusOK {
			t.Fatalf("%d names: status %d; body=%s", n, rec.Code, rec.Body.String())
		}
		var rows []protocol.NodeBinInfo
		testutil.MustNoErr(t, json.Unmarshal(rec.Body.Bytes(), &rows), "decode")
		if len(rows) != n {
			t.Fatalf("%d names: %d rows", n, len(rows))
		}
		for i, row := range rows {
			want := label[asked[i]]
			if row.NodeName != asked[i] || row.Occupied != (want != "") || row.BinLabel != want {
				t.Errorf("%d names: row %d = %s occupied=%v bin=%q, want %s bin=%q",
					n, i, row.NodeName, row.Occupied, row.BinLabel, asked[i], want)
			}
		}
		t.Logf("%d names: %d statements", n, count)
		if count != 2 {
			t.Errorf("%d names: %d statements, want 2 (one to resolve the names, one for their bins):\n  %s",
				n, count, strings.Join(stmts, "\n  "))
		}
	}
}
