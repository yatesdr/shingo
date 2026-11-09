package main

// lifecycle.go — the two state diagrams in docs/order-lifecycle.md, drawn
// from protocol's validTransitions.
//
//   - Edge's submission machine (`protocol/types.go validTransitions`): every
//     edge between the statuses an order born `pending` can reach. Edge
//     validates against the whole table, so this is the whole reachable table.
//     `sourcing` is a key nothing enters, so it is not drawn.
//   - Core's planning machine (`shingo-core/dispatch/lifecycle.go actionMap`):
//     the same edges, restricted to the statuses Core writes — `pending`, and
//     every status a LifecycleService method hands to transition() — each edge
//     labelled with the actions actionMap fires on it. An unlabelled edge is a
//     pure transition: status write and audit only.
//
// The pinned transition TABLE is docs/order-state-machine/transitions.md's
// (TestTransitionsDocTableMatchesTheCode); these are pictures of it.

import (
	"fmt"
	"go/ast"
	"strings"
)

func init() {
	register(Generator{Name: "protocol/types.go validTransitions", Emit: func(root string, args []string) (string, error) {
		if len(args) != 0 {
			return "", fmt.Errorf("takes no arguments, got %q", args)
		}
		m, err := lifecycleRead(root)
		if err != nil {
			return "", err
		}
		return m.diagram(nil, nil), nil
	}})
	register(Generator{Name: "shingo-core/dispatch/lifecycle.go actionMap", Emit: func(root string, args []string) (string, error) {
		if len(args) != 0 {
			return "", fmt.Errorf("takes no arguments, got %q", args)
		}
		m, err := lifecycleRead(root)
		if err != nil {
			return "", err
		}
		return m.diagram(m.coreWrites, m.actions), nil
	}})
}

// lifecycleBorn is the status every order is issued in (the ladder).
const lifecycleBorn = "pending"

// lifecycleMachine is the transition table, in source order, and Core's half
// of it.
type lifecycleMachine struct {
	froms      []string            // validTransitions' keys, in source order
	edges      map[string][]string // from → tos, in source order
	actions    map[[2]string][]string
	coreWrites map[string]bool
}

// lifecycleRead parses validTransitions, actionMap, and the statuses
// LifecycleService writes.
func lifecycleRead(root string) (*lifecycleMachine, error) {
	tree, err := newGosrcTree(root)
	if err != nil {
		return nil, err
	}
	proto, err := tree.pkg("protocol")
	if err != nil {
		return nil, err
	}
	dispatch, err := tree.pkg("shingo-core/dispatch")
	if err != nil {
		return nil, err
	}
	m := &lifecycleMachine{edges: map[string][]string{}, actions: map[[2]string][]string{}, coreWrites: map[string]bool{lifecycleBorn: true}}
	if err := m.readTransitions(proto); err != nil {
		return nil, err
	}
	if err := m.readActions(dispatch); err != nil {
		return nil, err
	}
	if err := m.readCoreWrites(dispatch); err != nil {
		return nil, err
	}
	return m, nil
}

// readTransitions reads protocol's validTransitions literal.
func (m *lifecycleMachine) readTransitions(proto *gosrcPkg) error {
	vt := proto.values["validTransitions"]
	if vt == nil {
		return fmt.Errorf("protocol declares no validTransitions")
	}
	lit, ok := vt.expr.(*ast.CompositeLit)
	if !ok {
		return fmt.Errorf("validTransitions is not a map literal")
	}
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			return fmt.Errorf("validTransitions: an entry is not key: value")
		}
		from, ok := vt.file.stringConst(kv.Key)
		if !ok {
			return fmt.Errorf("validTransitions: key %s is not a status constant", gosrcExpr(kv.Key))
		}
		tos, ok := kv.Value.(*ast.CompositeLit)
		if !ok {
			return fmt.Errorf("validTransitions[%s] is not a list literal", from)
		}
		m.froms = append(m.froms, from)
		for _, t := range tos.Elts {
			to, ok := vt.file.stringConst(t)
			if !ok {
				return fmt.Errorf("validTransitions[%s]: %s is not a status constant", from, gosrcExpr(t))
			}
			m.edges[from] = append(m.edges[from], to)
		}
	}
	return nil
}

// readActions reads dispatch's actionMap literal: transitionKey{from, to} →
// the actions it fires.
func (m *lifecycleMachine) readActions(dispatch *gosrcPkg) error {
	am := dispatch.values["actionMap"]
	if am == nil {
		return fmt.Errorf("shingo-core/dispatch declares no actionMap")
	}
	lit, ok := am.expr.(*ast.CompositeLit)
	if !ok {
		return fmt.Errorf("actionMap is not a map literal")
	}
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			return fmt.Errorf("actionMap: an entry is not key: value")
		}
		key, ok := kv.Key.(*ast.CompositeLit)
		if !ok {
			return fmt.Errorf("actionMap: key %s is not a transitionKey literal", gosrcExpr(kv.Key))
		}
		ends := map[string]string{}
		for _, f := range key.Elts {
			fkv, ok := f.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			s, ok := am.file.stringConst(fkv.Value)
			if !ok {
				return fmt.Errorf("actionMap: %s is not a status constant", gosrcExpr(fkv.Value))
			}
			ends[gosrcExpr(fkv.Key)] = s
		}
		edge := [2]string{ends["from"], ends["to"]}
		if edge[0] == "" || edge[1] == "" {
			return fmt.Errorf("actionMap: key %s names no from and to", gosrcExpr(kv.Key))
		}
		acts, ok := kv.Value.(*ast.CompositeLit)
		if !ok {
			return fmt.Errorf("actionMap[%s→%s] is not a list literal", edge[0], edge[1])
		}
		for _, a := range acts.Elts {
			m.actions[edge] = append(m.actions[edge], gosrcExpr(a))
		}
	}
	return nil
}

// readCoreWrites collects Core's statuses: every status constant a
// LifecycleService method hands to transition or transitionTx.
func (m *lifecycleMachine) readCoreWrites(dispatch *gosrcPkg) error {
	for _, fn := range dispatch.funcs {
		if fn.recv != "LifecycleService" || fn.decl.Body == nil {
			continue
		}
		ast.Inspect(fn.decl.Body, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := c.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "transition" && sel.Sel.Name != "transitionTx") {
				return true
			}
			for _, a := range c.Args {
				if s, ok := fn.file.stringConst(a); ok {
					m.coreWrites[s] = true
				}
			}
			return true
		})
	}
	if len(m.coreWrites) < 2 {
		return fmt.Errorf("no LifecycleService method calls transition with a status constant")
	}
	return nil
}

// reachable is every status an order born pending can reach.
func (m *lifecycleMachine) reachable() map[string]bool {
	seen := map[string]bool{lifecycleBorn: true}
	queue := []string{lifecycleBorn}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, to := range m.edges[cur] {
			if !seen[to] {
				seen[to] = true
				queue = append(queue, to)
			}
		}
	}
	return seen
}

// diagram draws the reachable machine as a mermaid state diagram. A non-nil
// only keeps the statuses it names; a non-nil labels writes each edge's
// actions on it.
func (m *lifecycleMachine) diagram(only map[string]bool, labels map[[2]string][]string) string {
	reach := m.reachable()
	drawn := func(s string) bool { return reach[s] && (only == nil || only[s]) }
	var b strings.Builder
	b.WriteString("```mermaid\nstateDiagram-v2\n")
	fmt.Fprintf(&b, "    [*] --> %s\n", lifecycleBorn)
	var terminals []string
	seenTerminal := map[string]bool{}
	for _, from := range m.froms {
		if !drawn(from) {
			continue
		}
		b.WriteString("\n")
		for _, to := range m.edges[from] {
			if !drawn(to) {
				continue
			}
			line := fmt.Sprintf("    %s --> %s", from, to)
			if acts := labels[[2]string{from, to}]; len(acts) > 0 {
				line += ": " + strings.Join(acts, ", ")
			}
			b.WriteString(line + "\n")
			if _, hasOut := m.edges[to]; !hasOut && !seenTerminal[to] {
				seenTerminal[to] = true
				terminals = append(terminals, to)
			}
		}
	}
	b.WriteString("\n")
	for _, t := range terminals {
		fmt.Fprintf(&b, "    %s --> [*]\n", t)
	}
	b.WriteString("```\n")
	return b.String()
}
