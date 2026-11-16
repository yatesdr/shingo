package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/internal/testdb"
	"shingoedge/store"
	"shingoedge/store/processes"
)

var updateAutoRequestPlans = flag.Bool("update-auto-request-plans", false,
	"rewrite testdata/auto-request-payload-plans.golden.txt")

// AUTO REQUEST PART MOVES NOTHING BUT THE FINGERPRINT. Nothing has read
// auto_request_payload since decffaa0 (2026-04-10). For every configurable
// swap mode and both roles, a process whose running style and target style
// both hold a value previews, plans and builds its steady state byte for byte
// as the same process with the field blank: the target's preview (actions,
// order count, findings, the steady steps), the running style's preview, and
// the desktop's changeover plan. The fingerprint is the one thing that moves.
//
// The golden holds the blank process's answers. It was written before the
// column went, and the removal must leave it as it is.
func TestAutoRequestPayload_OnlyTheFingerprintMoves(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	b.WriteString("# Every configurable mode x role: the target's preview, the running style's preview and the\n")
	b.WriteString("# desktop's changeover plan, fingerprints left out. The same with Auto request part set (asserted).\n")
	for _, mode := range protocol.ConfigurableSwapModes() {
		for _, role := range []protocol.ClaimRole{protocol.ClaimRoleConsume, protocol.ClaimRoleProduce} {
			name := string(mode) + "/" + string(role)
			blank := autoRequestPlans(t, mode, role, false)
			set := autoRequestPlans(t, mode, role, true)
			if blank.text != set.text {
				t.Errorf("%s: Auto request part moved a plan:\n blank %s\n set   %s", name, blank.text, set.text)
			}
			if blank.fingerprint == set.fingerprint {
				t.Errorf("%s: the fingerprint did not move with the field set; the pin no longer sets it", name)
			}
			b.WriteString("\n== " + name + "\n" + blank.text)
		}
	}
	got := []byte(b.String())
	path := filepath.Join("testdata", "auto-request-payload-plans.golden.txt")
	if *updateAutoRequestPlans {
		testutil.MustNoErr(t, os.WriteFile(path, got, 0o644), "write golden")
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no %s (%v); regenerate with -update-auto-request-plans", path, err)
	}
	if !bytes.Equal(bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")), got) {
		t.Errorf("%s moved: a plan, a preview or a steady step is not what it was; regenerate only for a named change", path)
	}
}

type autoRequestAnswer struct {
	text        string
	fingerprint string
}

// autoRequestPlans seeds one process in mode and role: a running style on
// PART-OLD and a target on PART-NEW at LINE (and its partner PAIR on a
// sequential line, PAIR being the press's back position on an index press),
// every required field set, with Auto request part on every claim when set.
func autoRequestPlans(t *testing.T, mode protocol.SwapMode, role protocol.ClaimRole, set bool) autoRequestAnswer {
	t.Helper()
	db, err := store.Open(testdb.Path(t))
	testutil.MustNoErr(t, err, "open")
	t.Cleanup(func() { _ = db.Close() })
	pid, err := db.CreateProcess("ARP", "auto request part", "", "", false)
	testutil.MustNoErr(t, err, "process")
	for i, n := range []string{"LINE", "PAIR"} {
		id, err := db.CreateProcessNode(processes.NodeInput{ProcessID: pid, CoreNodeName: n, Code: n, Name: n, Sequence: i + 1, Enabled: true})
		testutil.MustNoErr(t, err, "node "+n)
		db.EnsureProcessNodeRuntime(id)
	}
	from, err := db.CreateStyle("ARP-RUNNING", "", pid)
	testutil.MustNoErr(t, err, "running style")
	to, err := db.CreateStyle("ARP-TARGET", "", pid)
	testutil.MustNoErr(t, err, "target style")
	testutil.MustNoErr(t, db.SetActiveStyle(pid, &from), "active style")
	claim := func(style int64, node, partner, part string) {
		in := processes.NodeClaimInput{StyleID: style, CoreNodeName: node, Role: role, SwapMode: mode, PayloadCode: part,
			InboundSource: "ARP-SRC", OutboundDestination: "ARP-DST"}
		switch mode {
		case protocol.SwapModeSingleRobot:
			in.InboundStaging, in.OutboundStaging = "ARP-STG-IN", "ARP-STG-OUT"
		case protocol.SwapModeTwoRobot:
			in.InboundStaging = "ARP-STG-IN"
		case protocol.SwapModeTwoRobotPressIndex, protocol.SwapModeSequential:
			in.PairedCoreNode = partner
		}
		_, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, in)
		testutil.MustNoErr(t, err, "claim "+node)
	}
	for _, s := range []struct {
		id   int64
		part string
	}{{from, "PART-OLD"}, {to, "PART-NEW"}} {
		claim(s.id, "LINE", "PAIR", s.part)
		if mode == protocol.SwapModeSequential {
			claim(s.id, "PAIR", "LINE", s.part)
		}
	}
	if set {
		_, err := db.Exec(`UPDATE style_node_claims SET auto_request_payload = 'PART-AR'`)
		testutil.MustNoErr(t, err, "set Auto request part")
	}

	eng := testEngine(t, db)
	coreNeverAnswers(t, eng)
	preview := func(style int64) *FlowPreview {
		p, err := eng.PreviewFlow(context.Background(), pid, FlowPreviewRequest{ToStyleID: style, Cells: cellsOf(t, db, style), Steps: true})
		var none *FlowNoOrdersError
		if errors.As(err, &none) {
			return none.Preview
		}
		testutil.MustNoErr(t, err, "preview")
		return p
	}
	target, running := preview(to), preview(from)
	plan, err := eng.PreviewChangeoverPlan(pid, to)
	testutil.MustNoErr(t, err, "changeover plan")
	planJSON, err := json.Marshal(plan)
	testutil.MustNoErr(t, err, "marshal plan")
	return autoRequestAnswer{
		text: "target:  " + previewJSON(t, target) + "\nrunning: " + previewJSON(t, running) +
			"\nplan:    " + string(planJSON) + "\n",
		fingerprint: target.Fingerprint + running.Fingerprint,
	}
}
