package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// ONE LINE, ONE LIVE SWAP. A second REQUEST while the line's swap is still
// working the cell is refused, whatever the swap mode that runs it; the
// dry-source refusal stays with the shapes that arm two robots at once.

const (
	osLine   = "OS-LINE"
	osPart   = "PART-OS"
	osMarket = "OS-MARKET"
)

// oneSwapCell seeds a line with one claim and a Core that reports the line
// occupied and, when dry, no bin of the claim's part anywhere.
func oneSwapCell(t *testing.T, role protocol.ClaimRole, mode protocol.SwapMode, dry bool) (*Engine, int64) {
	t.Helper()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.logFn = func(string, ...any) {}

	procID, err := db.CreateProcess("OS-PROC", "", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "process")
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: procID, CoreNodeName: osLine, Code: "OS1", Name: osLine, Sequence: 1, Enabled: true,
	})
	testutil.MustNoErr(t, err, "node")
	styleID, err := db.CreateStyle("OS-STYLE", "", procID)
	testutil.MustNoErr(t, err, "style")
	testutil.MustNoErr(t, db.SetActiveStyle(procID, &styleID), "active style")
	in := processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: osLine, Role: role, SwapMode: mode, PayloadCode: osPart,
		UOPCapacity: 40, InboundSource: osMarket, InboundStaging: "OS-IN", OutboundDestination: "OS-DEST",
	}
	if mode == protocol.SwapModeSingleRobot {
		in.OutboundStaging = "OS-OUT"
	}
	claimID, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, in)
	testutil.MustNoErr(t, err, "claim")
	_, err = db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "runtime")
	testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &claimID, 5), "runtime claim")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/telemetry/node-bins":
			out := []NodeBinInfo{}
			for _, n := range strings.Split(r.URL.Query().Get("nodes"), ",") {
				if n != "" {
					out = append(out, NodeBinInfo{NodeName: n, Occupied: n == osLine, PayloadCode: osPart})
				}
			}
			_ = json.NewEncoder(w).Encode(out)
		case "/api/inventory/preflight":
			absent := []string{}
			if dry {
				absent = []string{osPart}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"missing": absent, "absent": absent})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	eng.coreClient = stubCoreClient(srv.URL)
	return eng, nodeID
}

func oneSwapRequest(eng *Engine, role protocol.ClaimRole, nodeID int64) error {
	if role == protocol.ClaimRoleProduce {
		_, err := eng.RequestProduceSwap(nodeID)
		return err
	}
	_, err := eng.RequestNodeMaterial(nodeID, 1)
	return err
}

func TestOneLiveSwap_SecondRequestIsRefused(t *testing.T) {
	t.Parallel()
	for _, mode := range []protocol.SwapMode{protocol.SwapModeSingleRobot, protocol.SwapModeTwoRobot} {
		for _, role := range []protocol.ClaimRole{protocol.ClaimRoleConsume, protocol.ClaimRoleProduce} {
			t.Run(string(mode)+"/"+string(role), func(t *testing.T) {
				t.Parallel()
				eng, nodeID := oneSwapCell(t, role, mode, false)
				testutil.MustNoErr(t, oneSwapRequest(eng, role, nodeID), "first REQUEST")
				err := oneSwapRequest(eng, role, nodeID)
				if err == nil || !strings.Contains(err.Error(), "already in progress") {
					t.Fatalf("second REQUEST while the first swap is live: err = %v, want refused as already in progress", err)
				}
			})
		}
	}
}

// The dry-source refusal is for arming two robots at once. A single-robot
// swap is one order and is created against a dry part as before, to wait for
// stock; a two-robot pair is not armed.
func TestOneLiveSwap_DrySourceRefusesOnlyAPair(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		mode    protocol.SwapMode
		refused bool
	}{
		{protocol.SwapModeSingleRobot, false},
		{protocol.SwapModeTwoRobot, true},
	} {
		t.Run(string(c.mode), func(t *testing.T) {
			t.Parallel()
			eng, nodeID := oneSwapCell(t, protocol.ClaimRoleConsume, c.mode, true)
			err := oneSwapRequest(eng, protocol.ClaimRoleConsume, nodeID)
			if got := err != nil && strings.Contains(err.Error(), "no "+osPart+" bin anywhere"); got != c.refused {
				t.Fatalf("REQUEST against a dry part: err = %v, want refused=%v", err, c.refused)
			}
		})
	}
}
