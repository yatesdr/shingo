package main

import (
	"strings"
	"testing"
)

// lifecycleFixture is a five-status machine: `ghost` is a key nothing enters,
// `edgeonly` a status no LifecycleService method writes.
var lifecycleFixture = map[string]string{
	"go.work":            "go 1.25\n\nuse (\n\t./protocol\n\t./shingo-core\n)\n",
	"protocol/go.mod":    "module shingo/protocol\n",
	"shingo-core/go.mod": "module shingocore\n",
	"protocol/status.go": `package protocol

type Status string

const (
	StatusPending  Status = "pending"
	StatusEdgeOnly Status = "edgeonly"
	StatusGhost    Status = "ghost"
	StatusMoving   Status = "moving"
	StatusDone     Status = "done"
)
`,
	"protocol/types.go": `package protocol

var validTransitions = map[Status][]Status{
	StatusPending:  {StatusEdgeOnly, StatusMoving},
	StatusEdgeOnly: {StatusMoving},
	StatusGhost:    {StatusMoving},
	StatusMoving:   {StatusDone},
}
`,
	"shingo-core/dispatch/lifecycle.go": `package dispatch

import "shingo/protocol"

const (
	StatusMoving = protocol.StatusMoving
	StatusDone   = protocol.StatusDone
)

type transitionKey struct{ from, to protocol.Status }

var actionMap = map[transitionKey][]Action{
	{from: StatusMoving, to: StatusDone}: {fireDone},
}

type LifecycleService struct{}

func (s *LifecycleService) transition(to protocol.Status) error { return nil }

func (s *LifecycleService) Move() error { return s.transition(StatusMoving) }
func (s *LifecycleService) Finish() error { return s.transition(StatusDone) }
`,
}

func TestLifecycle_DrawsTheReachableMachine(t *testing.T) {
	root := gosrcWriteTree(t, lifecycleFixture)
	edge, err := generators["protocol/types.go validTransitions"].Emit(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	core, err := generators["shingo-core/dispatch/lifecycle.go actionMap"].Emit(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, got string
		want      []string
		not       []string
	}{
		{"edge", edge,
			[]string{"[*] --> pending", "pending --> edgeonly", "edgeonly --> moving", "moving --> done", "done --> [*]"},
			[]string{"ghost", ": fireDone"}},
		{"core", core,
			[]string{"[*] --> pending", "pending --> moving", "moving --> done: fireDone", "done --> [*]"},
			[]string{"ghost", "edgeonly"}},
	} {
		if !strings.HasPrefix(c.got, "```mermaid\nstateDiagram-v2\n") {
			t.Errorf("%s: not a mermaid state diagram:\n%s", c.name, c.got)
		}
		for _, w := range c.want {
			if !strings.Contains(c.got, w+"\n") {
				t.Errorf("%s: no line %q in\n%s", c.name, w, c.got)
			}
		}
		for _, n := range c.not {
			if strings.Contains(c.got, n) {
				t.Errorf("%s: draws %q:\n%s", c.name, n, c.got)
			}
		}
	}
}

// TestLifecycle_TheRealMachine pins the tree's diagrams on the facts the doc's
// notes state: Core does not draw submitted, neither draws sourcing, and the
// edges the hand-drawn copies had lost are drawn.
func TestLifecycle_TheRealMachine(t *testing.T) {
	core, err := generators["shingo-core/dispatch/lifecycle.go actionMap"].Emit(repoRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	edge, err := generators["protocol/types.go validTransitions"].Emit(repoRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"in_transit --> delivered: fireCompleted", "reshuffling --> pending: fireResumed", "dispatched --> staged", "dispatched --> queued"} {
		if !strings.Contains(core, "    "+w+"\n") {
			t.Errorf("core: no edge %q", w)
		}
	}
	for _, n := range []string{"submitted", "sourcing"} {
		if strings.Contains(core, n) {
			t.Errorf("core draws %s", n)
		}
	}
	if !strings.Contains(edge, "    pending --> submitted\n") || strings.Contains(edge, "sourcing") {
		t.Errorf("edge: want pending --> submitted and no sourcing:\n%s", edge)
	}
}
