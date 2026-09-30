package engine

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"shingoedge/plc"
	"shingoedge/store/processes"
)

// curtain_gate_test.go - the FG light-curtain release interlock's gate.
//
// The gate is fail-closed on everything, and the ladder below walks every
// refusal: the role key (consume is never gated), the toggle, the missing
// pointers, the unreadable tag, a value that is not a BOOL, the wrong
// reading, and the pass. The WarLink client is a stub whose ReadTagValue
// answers what the case needs - the gate reads DIRECTLY through it, never
// the poll cache, so the stub is the freshest answer there is.

type curtainStubClient struct {
	value any
	err   error
}

func (c *curtainStubClient) ListPLCs(ctx context.Context) ([]plc.WarlinkPLC, error) {
	return nil, nil
}
func (c *curtainStubClient) ListTags(ctx context.Context, plcName string) (map[string]plc.WarlinkTag, error) {
	return nil, nil
}
func (c *curtainStubClient) ListAllTags(ctx context.Context, plcName string) ([]plc.WarlinkTagInfo, error) {
	return nil, nil
}
func (c *curtainStubClient) SetTagPublishing(ctx context.Context, plcName, tagName string, enabled bool) error {
	return nil
}
func (c *curtainStubClient) ReadTagValue(ctx context.Context, plcName, tagName string) (any, error) {
	if c.err != nil {
		return nil, c.err
	}
	return c.value, nil
}
func (c *curtainStubClient) WriteTagValue(ctx context.Context, plcName, tagName string, value any) error {
	return nil
}
func (c *curtainStubClient) OpenEventStream(ctx context.Context) (io.ReadCloser, error) {
	return nil, errors.New("no stream in this stub")
}

// curtainSeedNode inserts a process and one node, writes the node's
// interlock columns, and returns the node READ BACK - so each case also
// proves the scan carries what the gate reads. Rows the setter would refuse
// (enabled with a pointer or the polarity missing) are written directly: the
// gate must refuse them anyway, whatever wrote them.
func curtainSeedNode(t *testing.T, eng *Engine, enabled bool, plcName, tagName string, safe *bool) *processes.Node {
	t.Helper()
	procID, err := eng.db.CreateProcess("CurtainProc", "", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err := eng.db.CreateProcessNode(processes.NodeInput{ProcessID: procID, CoreNodeName: "SYN-FG-1", Name: "SYN-FG-1", Enabled: true})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	if enabled && (plcName == "" || tagName == "" || safe == nil) {
		if _, err := eng.db.Exec(`UPDATE process_nodes SET curtain_enabled=1, curtain_plc_name=?, curtain_tag_name=?, curtain_safe_value=? WHERE id=?`,
			plcName, tagName, safe, nodeID); err != nil {
			t.Fatalf("write curtain row: %v", err)
		}
	} else if err := eng.db.SetProcessNodeCurtain(nodeID, enabled, plcName, tagName, safe); err != nil {
		t.Fatalf("set curtain: %v", err)
	}
	node, err := eng.db.GetProcessNode(nodeID)
	if err != nil {
		t.Fatalf("read node: %v", err)
	}
	return node
}

func boolPtr(v bool) *bool { return &v }

func curtainGateFixture(t *testing.T, client plc.WarlinkClient) (*Engine, *processes.Node) {
	t.Helper()
	db := testEngineDB(t)
	eng := &Engine{db: db, plcMgr: plc.NewManager(nil, nil, nil, client),
		logFn: func(string, ...any) {}, debugFn: func(string, ...any) {}}
	return eng, nil
}

// gateNode runs the gate the way a door does for the node a bin leaves, in a
// fresh act.
func gateNode(eng *Engine, node *processes.Node) error {
	return eng.curtainForNode(newReleaseAct(), node.CoreNodeName)
}

// TestCurtainGate walks the refusal ladder. Each case names what the
// operator's screen will say, because these strings are the floor's
// answer to "why won't it release".
func TestCurtainGate(t *testing.T) {
	t.Run("a curtained node is gated whatever the release's claim", func(t *testing.T) {
		// Per node, not per role: the node's own setting says it has a
		// curtain, and a consume release through it crosses it like any other.
		client := &curtainStubClient{value: true}
		eng, _ := curtainGateFixture(t, client)
		node := curtainSeedNode(t, eng, true, "PRESS-PLC", "FG_CURTAIN", boolPtr(false))
		if err := gateNode(eng, node); err == nil {
			t.Fatal("a curtained node released with its curtain live")
		}
	})

	t.Run("the interlock off releases as always", func(t *testing.T) {
		client := &curtainStubClient{value: false}
		eng, _ := curtainGateFixture(t, client)
		node := curtainSeedNode(t, eng, false, "", "", boolPtr(false))
		if err := gateNode(eng, node); err != nil {
			t.Fatalf("interlock-off release refused: %v", err)
		}
	})

	t.Run("the safe reading releases", func(t *testing.T) {
		client := &curtainStubClient{value: true}
		eng, _ := curtainGateFixture(t, client)
		node := curtainSeedNode(t, eng, true, "PRESS-PLC", "FG_CURTAIN", boolPtr(true))
		if err := gateNode(eng, node); err != nil {
			t.Fatalf("safe reading refused: %v", err)
		}
	})

	t.Run("the unsafe reading refuses with the plain sentence", func(t *testing.T) {
		client := &curtainStubClient{value: false}
		eng, _ := curtainGateFixture(t, client)
		node := curtainSeedNode(t, eng, true, "PRESS-PLC", "FG_CURTAIN", boolPtr(true))
		err := gateNode(eng, node)
		if err == nil {
			t.Fatal("unsafe reading released")
		}
		if err.Error() != "Release the light curtain at SYN-FG-1, then press RELEASE again." {
			t.Fatalf("refusal = %q, want the plain sentence naming the node", err)
		}
	})

	t.Run("an unreadable tag refuses, fail-closed", func(t *testing.T) {
		client := &curtainStubClient{err: errors.New("WarLink GET returned 503")}
		eng, _ := curtainGateFixture(t, client)
		node := curtainSeedNode(t, eng, true, "PRESS-PLC", "FG_CURTAIN", boolPtr(true))
		err := gateNode(eng, node)
		if err == nil {
			t.Fatal("unreadable tag released")
		}
		if !strings.Contains(err.Error(), "could not be read") {
			t.Fatalf("refusal does not name the read failure: %v", err)
		}
	})

	t.Run("enabled with no pointers refuses", func(t *testing.T) {
		client := &curtainStubClient{value: true}
		eng, _ := curtainGateFixture(t, client)
		node := curtainSeedNode(t, eng, true, "", "", boolPtr(true))
		err := gateNode(eng, node)
		if err == nil {
			t.Fatal("pointer-less interlock released")
		}
		if !strings.Contains(err.Error(), "PLC/tag are not set") {
			t.Fatalf("refusal does not name the config gap: %v", err)
		}
	})

	t.Run("enabled with no chosen polarity refuses, naming the setting", func(t *testing.T) {
		client := &curtainStubClient{value: false}
		eng, _ := curtainGateFixture(t, client)
		node := curtainSeedNode(t, eng, true, "PRESS-PLC", "FG_CURTAIN", nil)
		err := gateNode(eng, node)
		if err == nil {
			t.Fatal("an interlock with no polarity released")
		}
		if !strings.Contains(err.Error(), "nobody has chosen which tag value allows the release") {
			t.Fatalf("refusal does not name the missing polarity: %v", err)
		}
	})

	t.Run("the FALSE polarity releases on FALSE", func(t *testing.T) {
		client := &curtainStubClient{value: false}
		eng, _ := curtainGateFixture(t, client)
		node := curtainSeedNode(t, eng, true, "PRESS-PLC", "FG_CURTAIN", boolPtr(false))
		if err := gateNode(eng, node); err != nil {
			t.Fatalf("FALSE-polarity safe reading refused: %v", err)
		}
	})

	t.Run("a value that is not a BOOL refuses, not guesses", func(t *testing.T) {
		client := &curtainStubClient{value: "MID-TRAVEL"}
		eng, _ := curtainGateFixture(t, client)
		node := curtainSeedNode(t, eng, true, "PRESS-PLC", "FG_CURTAIN", boolPtr(true))
		err := gateNode(eng, node)
		if err == nil {
			t.Fatal("non-BOOL value released")
		}
		if !strings.Contains(err.Error(), "not a BOOL") {
			t.Fatalf("refusal does not name the value problem: %v", err)
		}
	})

	t.Run("one act reads a node once", func(t *testing.T) {
		// The act's memo is what keeps a pair click from passing the check for
		// one leg and failing it for the other, and what keeps the per-leg
		// release below a door's paperwork from reading again.
		wl := &scriptedWarLink{seq: []any{true, false}}
		eng, _ := curtainGateFixture(t, wl)
		node := curtainSeedNode(t, eng, true, "PRESS-PLC", "FG_CURTAIN", boolPtr(true))
		act := newReleaseAct()
		for i := 0; i < 3; i++ {
			if err := eng.curtainForNode(act, node.CoreNodeName); err != nil {
				t.Fatalf("read %d: %v", i, err)
			}
		}
		if wl.count() != 1 {
			t.Fatalf("one act read the curtain %d times, want 1", wl.count())
		}
	})
}

// TestCurtainBool pins the interpreter's tolerance: the PLC's BOOL as JSON
// bool, the 0/1 numeric spellings, the "0"/"1" strings - and REFUSES
// everything else, because a safety gate that guesses has already failed.
func TestCurtainBool(t *testing.T) {
	cases := []struct {
		raw  any
		want bool
		ok   bool
	}{
		{true, true, true},
		{false, false, true},
		{1, true, true},
		{0, false, true},
		{int64(1), true, true},
		{float64(1), true, true},
		{float64(0), false, true},
		{"1", true, true},
		{"0", false, true},
		{" 1 ", true, true},
		{2, false, false},
		{"on", false, false},
		{nil, false, false},
	}
	for _, c := range cases {
		got, ok := plc.CurtainBool(c.raw)
		if got != c.want || ok != c.ok {
			t.Errorf("CurtainBool(%v) = %v, %v; want %v, %v", c.raw, got, ok, c.want, c.ok)
		}
	}
}
