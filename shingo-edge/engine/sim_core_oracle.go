//go:build sim

package engine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// sim_core_oracle.go — the sim operator's view of the simulated floor itself,
// read from Core's own node list and bin read (GET /api/nodes, GET
// /api/bins/by-node: the reads Core's pages make).
//
// TEST TOOLING, NOT A TRANSPORT. The sim operator plays a person standing in
// the plant. Where it needs to know what physically exists — a market's lanes
// and their slots, the parts a person counts in a bin, a negative count left
// on a market slot — it reads Core, which is the simulated floor; reading the
// Edge's own copy would certify only what the Edge already believes. What a
// person reads off the board (whether a carrier stands there, labelled or
// stocked) the operator reads from the board's source, the Edge's bins table. A
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
	n, ok := t.resolve(name)
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

// resolve is the node called name: by its own name, or, dotted, by its last
// part under its first, as Core resolves one.
func (t *simNodeTree) resolve(name string) (simNodeTreeRow, bool) {
	if n, ok := t.byName[name]; ok {
		return n, true
	}
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '.' {
			parent, child := name[:i], name[i+1:]
			if p, okP := t.byName[parent]; okP {
				for _, c := range t.children[p.ID] {
					if c.Name == child {
						return c, true
					}
				}
			}
			break
		}
	}
	return simNodeTreeRow{}, false
}

// simFloorBin is the carrier physically at a node of the simulated floor, as
// Core holds it: its id, its part and its count. The count is the floor's
// truth that no Edge-facing row carries (the bins table carries none): a
// person counting the parts in front of them, or the negative count a weld
// overpack leaves.
type simFloorBin struct {
	Occupied     bool
	BinID        int64
	PayloadCode  string
	UOPRemaining int
}

// readSimFloorBins reads the carrier at each named node from Core's own bin
// read (GET /api/bins/by-node, the read Core's pages make), with one read of
// Core's node list to resolve the names: the sim operator's oracle for what
// physically stands there. A name Core has no node for is absent; a node with
// no carrier is present and unoccupied. Test tooling: absent from every
// non-sim build.
func readSimFloorBins(baseURL string, names []string) (map[string]simFloorBin, error) {
	tree, err := readSimNodeTree(baseURL)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	out := make(map[string]simFloorBin, len(names))
	for _, name := range names {
		n, ok := tree.resolve(name)
		if !ok {
			continue
		}
		resp, err := client.Get(fmt.Sprintf("%s/api/bins/by-node?id=%d", baseURL, n.ID))
		if err != nil {
			return nil, err
		}
		var bins []struct {
			ID           int64  `json:"id"`
			PayloadCode  string `json:"payload_code"`
			UOPRemaining int    `json:"uop_remaining"`
		}
		decErr := json.NewDecoder(resp.Body).Decode(&bins)
		status := resp.StatusCode
		resp.Body.Close()
		if status != http.StatusOK {
			return nil, fmt.Errorf("core bins at %s: HTTP %d", name, status)
		}
		if decErr != nil {
			return nil, decErr
		}
		b := simFloorBin{}
		if len(bins) > 0 { // newest first: the resident carrier
			b = simFloorBin{Occupied: true, BinID: bins[0].ID, PayloadCode: bins[0].PayloadCode, UOPRemaining: bins[0].UOPRemaining}
		}
		out[name] = b
	}
	return out, nil
}
