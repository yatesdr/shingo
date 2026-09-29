package engine

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"shingo/protocol"
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

// curtainSeedProcess inserts a process row and writes the interlock's
// columns through the side-door path the settings use.
func curtainSeedProcess(t *testing.T, eng *Engine, enabled bool, plcName, tagName string, safe bool) int64 {
	t.Helper()
	id, err := eng.db.CreateProcess("CurtainProc", "", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	if err := eng.db.SetProcessCurtain(id, enabled, plcName, tagName, safe); err != nil {
		t.Fatalf("set curtain: %v", err)
	}
	return id
}

func curtainGateFixture(t *testing.T, client plc.WarlinkClient) (*Engine, *processes.Node) {
	t.Helper()
	db := testEngineDB(t)
	eng := &Engine{db: db, plcMgr: plc.NewManager(nil, nil, nil, client),
		logFn: func(string, ...any) {}, debugFn: func(string, ...any) {}}
	return eng, nil
}

// TestCurtainGate walks the refusal ladder. Each case names what the
// operator's screen will say, because these strings are the floor's
// answer to "why won't it release".
func TestCurtainGate(t *testing.T) {
	t.Run("a consume claim is never gated, whatever the process says", func(t *testing.T) {
		client := &curtainStubClient{value: false}
		eng, _ := curtainGateFixture(t, client)
		procID := curtainSeedProcess(t, eng, true, "PRESS-PLC", "FG_CURTAIN", false)
		node := &processes.Node{ProcessID: procID, Name: "SMN-1", CoreNodeName: "SMN-1"}
		claim := &processes.NodeClaim{Role: protocol.ClaimRoleConsume}
		if err := eng.curtainGate(node, claim); err != nil {
			t.Fatalf("consume release gated: %v", err)
		}
	})

	t.Run("the interlock off releases as always", func(t *testing.T) {
		client := &curtainStubClient{value: false}
		eng, _ := curtainGateFixture(t, client)
		procID := curtainSeedProcess(t, eng, false, "", "", false)
		node := &processes.Node{ProcessID: procID, Name: "SMN-1", CoreNodeName: "SMN-1"}
		claim := &processes.NodeClaim{Role: protocol.ClaimRoleProduce}
		if err := eng.curtainGate(node, claim); err != nil {
			t.Fatalf("interlock-off release refused: %v", err)
		}
	})

	t.Run("the safe reading releases", func(t *testing.T) {
		client := &curtainStubClient{value: true}
		eng, _ := curtainGateFixture(t, client)
		procID := curtainSeedProcess(t, eng, true, "PRESS-PLC", "FG_CURTAIN", true)
		node := &processes.Node{ProcessID: procID, Name: "SMN-1", CoreNodeName: "SMN-1"}
		claim := &processes.NodeClaim{Role: protocol.ClaimRoleProduce}
		if err := eng.curtainGate(node, claim); err != nil {
			t.Fatalf("safe reading refused: %v", err)
		}
	})

	t.Run("the unsafe reading refuses, naming both sides", func(t *testing.T) {
		client := &curtainStubClient{value: false}
		eng, _ := curtainGateFixture(t, client)
		procID := curtainSeedProcess(t, eng, true, "PRESS-PLC", "FG_CURTAIN", true)
		node := &processes.Node{ProcessID: procID, Name: "SMN-1", CoreNodeName: "SMN-1"}
		claim := &processes.NodeClaim{Role: protocol.ClaimRoleProduce}
		err := eng.curtainGate(node, claim)
		if err == nil {
			t.Fatal("unsafe reading released")
		}
		if !strings.Contains(err.Error(), "not in its release state") || !strings.Contains(err.Error(), "release requires TRUE") {
			t.Fatalf("refusal does not name the mismatch: %v", err)
		}
	})

	t.Run("an unreadable tag refuses, fail-closed", func(t *testing.T) {
		client := &curtainStubClient{err: errors.New("WarLink GET returned 503")}
		eng, _ := curtainGateFixture(t, client)
		procID := curtainSeedProcess(t, eng, true, "PRESS-PLC", "FG_CURTAIN", true)
		node := &processes.Node{ProcessID: procID, Name: "SMN-1", CoreNodeName: "SMN-1"}
		claim := &processes.NodeClaim{Role: protocol.ClaimRoleProduce}
		err := eng.curtainGate(node, claim)
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
		procID := curtainSeedProcess(t, eng, true, "", "", true)
		node := &processes.Node{ProcessID: procID, Name: "SMN-1", CoreNodeName: "SMN-1"}
		claim := &processes.NodeClaim{Role: protocol.ClaimRoleProduce}
		err := eng.curtainGate(node, claim)
		if err == nil {
			t.Fatal("pointer-less interlock released")
		}
		if !strings.Contains(err.Error(), "pointers are missing") {
			t.Fatalf("refusal does not name the config gap: %v", err)
		}
	})

	t.Run("a value that is not a BOOL refuses, not guesses", func(t *testing.T) {
		client := &curtainStubClient{value: "MID-TRAVEL"}
		eng, _ := curtainGateFixture(t, client)
		procID := curtainSeedProcess(t, eng, true, "PRESS-PLC", "FG_CURTAIN", true)
		node := &processes.Node{ProcessID: procID, Name: "SMN-1", CoreNodeName: "SMN-1"}
		claim := &processes.NodeClaim{Role: protocol.ClaimRoleProduce}
		err := eng.curtainGate(node, claim)
		if err == nil {
			t.Fatal("non-BOOL value released")
		}
		if !strings.Contains(err.Error(), "did not read as a BOOL") {
			t.Fatalf("refusal does not name the value problem: %v", err)
		}
	})

	t.Run("a nil claim is not a produce release", func(t *testing.T) {
		client := &curtainStubClient{value: false}
		eng, _ := curtainGateFixture(t, client)
		procID := curtainSeedProcess(t, eng, true, "PRESS-PLC", "FG_CURTAIN", true)
		node := &processes.Node{ProcessID: procID, Name: "SMN-1", CoreNodeName: "SMN-1"}
		if err := eng.curtainGate(node, nil); err != nil {
			t.Fatalf("claim-less release gated: %v", err)
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
