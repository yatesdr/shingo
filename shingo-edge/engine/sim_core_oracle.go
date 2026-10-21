//go:build sim

package engine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// sim_core_oracle.go — the sim operator's view of the simulated floor itself,
// read from Core's own node list (GET /api/nodes, the read Core's pages make).
//
// TEST TOOLING, NOT A TRANSPORT. The sim operator plays a person standing in
// the plant. Where it needs to know what physically exists — a market's lanes
// and their slots — it reads Core, which is the simulated floor; reading the
// Edge's own copy would certify only what the Edge already believes. A
// market's lanes are synthetic nodes, which no Edge-facing read carries
// (Core's group rows hold a group's non-synthetic children only), so this is
// the one place the walk can come from. Absent from every non-sim build.

// simNodeTreeRow is the part of one Core node the walk reads.
type simNodeTreeRow struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	IsSynthetic bool   `json:"is_synthetic"`
	ParentID    *int64 `json:"parent_id,omitempty"`
}

// simNodeTree is Core's nodes by name and the children of each node, in
// Core's order.
type simNodeTree struct {
	byName   map[string]simNodeTreeRow
	children map[int64][]simNodeTreeRow
}

// readSimNodeTree reads Core's whole node list once.
func readSimNodeTree(baseURL string) (*simNodeTree, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("no Core configured")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(baseURL + "/api/nodes")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("core nodes: HTTP %d", resp.StatusCode)
	}
	var rows []simNodeTreeRow
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, err
	}
	t := &simNodeTree{byName: make(map[string]simNodeTreeRow, len(rows)), children: map[int64][]simNodeTreeRow{}}
	for _, r := range rows {
		t.byName[r.Name] = r
		if r.ParentID != nil {
			t.children[*r.ParentID] = append(t.children[*r.ParentID], r)
		}
	}
	return t, nil
}

// childNames is the children of the node called name, each qualified
// "<name's own node name>.<child>", synthetic ones included or not: the names
// Core's telemetry read answered with. A dotted name resolves by its last
// part under its first, as Core resolves one.
func (t *simNodeTree) childNames(name string, includeSynthetic bool) []string {
	n, ok := t.byName[name]
	if !ok {
		for i := len(name) - 1; i >= 0; i-- {
			if name[i] == '.' {
				parent, child := name[:i], name[i+1:]
				if p, okP := t.byName[parent]; okP {
					for _, c := range t.children[p.ID] {
						if c.Name == child {
							n, ok = c, true
						}
					}
				}
				break
			}
		}
	}
	if !ok {
		return nil
	}
	var out []string
	for _, c := range t.children[n.ID] {
		if includeSynthetic || !c.IsSynthetic {
			out = append(out, n.Name+"."+c.Name)
		}
	}
	return out
}
