package service

import (
	"fmt"
	"strings"

	"shingoedge/store/stations"
)

// A Core loader with no operator board is invisible from both ends. Core owns
// the loader and cannot see whether a board exists — plant.claims carries
// processes, styles and claims, and nothing reports stations up — while the Edge
// draws a board only where somebody has put the loader's windows into one of its
// processes by hand. Springfield 2026-08-26: the loader was correct on Core and
// there was no screen, and neither screen said so.
//
// WHY THIS LIVES ON THE EDGE, having started as a Core idea. Core would need a
// new uplink message to learn what boards exist AND a command channel to create
// one — every Core→Edge subject today is a state push or a reply. The Edge
// already holds every input in local SQLite: the loaders Core sent down, their
// windows, and what this edge has bound to which screen.
//
// WHY IT DOES NOT AUTO-PROVISION. Core sends every loader to every edge —
// BuildLoaderInfos takes no edge argument and the node list is unfiltered — so
// the ONLY thing assigning a loader window to an edge is a human putting that
// node into one of that edge's processes. Creating boards automatically would
// remove that decision without replacing it, and the never-2N budget is per-edge
// (withLoaderBudget holds an in-process mutex over the local order table), so two
// edges drawing one loader would each grant themselves a full window allowance.
// The button keeps the human answering "which edge, which process" and only
// removes the tedium of answering it across four screens.

// LoaderBoardGap is a Core loader this edge draws NO operator board for.
type LoaderBoardGap struct {
	LoaderKey string   `json:"loader_key"`
	Name      string   `json:"name"`
	Role      string   `json:"role"`
	Layout    string   `json:"layout"`
	Windows   []string `json:"windows"`
	// ProcessID names the process already holding this loader's windows as
	// process_nodes, when exactly one does — the common "the nodes are modelled
	// but nobody made the screen" case, where the answer is not a guess. Zero
	// when no process holds them, or more than one does; then the caller must ask.
	ProcessID   int64  `json:"process_id,omitempty"`
	ProcessName string `json:"process_name,omitempty"`
}

// nodeBinding is where one core node sits on this edge.
type nodeBinding struct {
	processID   int64
	processName string
	onStation   bool
}

// LoaderBoardGaps lists Core loaders with no board on this edge.
//
// ONLY a loader with ZERO of its windows on a station counts, and that threshold
// is the whole design. Partial binding is NORMAL, not a gap: Springfield's
// dedicated supermarket loader has 33 positions of which 22 are on the Bin Loader
// board and 11 are buffer slots that no operator ever works. Reporting "unbound
// windows" would put a permanent 11-row complaint on a correctly configured
// loader — the same failure mode as a threshold warning that fires on every
// loader that does not use thresholds.
//
// A loader with no windows at all is skipped too. There is nothing to bind, and
// Core's own box already says so (configGapHtml) at the place it can be fixed.
func (s *StationService) LoaderBoardGaps() ([]LoaderBoardGap, error) {
	loaders, err := s.db.ListCoreLoaders()
	if err != nil {
		return nil, fmt.Errorf("list core loaders: %w", err)
	}
	bindings, err := s.nodeBindings()
	if err != nil {
		return nil, err
	}

	var out []LoaderBoardGap
	for _, l := range loaders {
		windows := make([]string, 0, len(l.Positions))
		for _, p := range l.Positions {
			if p.PositionNode != "" {
				windows = append(windows, p.PositionNode)
			}
		}
		if len(windows) == 0 {
			continue
		}
		gap := LoaderBoardGap{
			LoaderKey: l.LoaderKey, Name: l.Name,
			Role: l.Role, Layout: l.Layout, Windows: windows,
		}
		boarded := false
		procs := map[int64]string{}
		for _, w := range windows {
			b, ok := bindings[w]
			if !ok {
				continue
			}
			if b.onStation {
				boarded = true
				break
			}
			procs[b.processID] = b.processName
		}
		if boarded {
			continue
		}
		// Only an unambiguous answer is offered. Two processes holding one
		// loader's windows is a real (if odd) shape, and picking one for the
		// operator would be inventing the ownership decision this whole surface
		// exists to keep with them.
		if len(procs) == 1 {
			for id, name := range procs {
				gap.ProcessID, gap.ProcessName = id, name
			}
		}
		out = append(out, gap)
	}
	return out, nil
}

// nodeBindings maps every live process_node's core node name to where it sits.
func (s *StationService) nodeBindings() (map[string]nodeBinding, error) {
	rows, err := s.db.Query(`SELECT n.core_node_name, n.process_id, p.name,
		CASE WHEN n.operator_station_id IS NULL THEN 0 ELSE 1 END
		FROM process_nodes n
		JOIN processes p ON p.id = n.process_id
		WHERE n.deleted_at IS NULL AND n.core_node_name <> ''`)
	if err != nil {
		return nil, fmt.Errorf("node bindings: %w", err)
	}
	defer rows.Close()
	out := map[string]nodeBinding{}
	for rows.Next() {
		var name, procName string
		var procID int64
		var onStation int
		if err := rows.Scan(&name, &procID, &procName, &onStation); err != nil {
			return nil, fmt.Errorf("node bindings: %w", err)
		}
		// A node already seen ON a station wins: UNIQUE(process_id,
		// core_node_name) is per process, so one Core node can legitimately be
		// modelled in two processes and only one of them may carry the screen.
		if prev, ok := out[name]; ok && prev.onStation {
			continue
		}
		out[name] = nodeBinding{processID: procID, processName: procName, onStation: onStation == 1}
	}
	return out, rows.Err()
}

// CreateLoaderBoard makes the operator screen for a Core loader and binds its
// windows to it, in that order, as one action.
//
// One call rather than the two it composes (create station, then set its nodes)
// because a half-applied version — a screen with no nodes — is exactly the
// looks-configured-does-nothing state this work exists to remove.
//
// Node binding goes through SetNodes rather than inserting rows, so it inherits
// both of that function's contracts: names are validated against the live Core
// node set, and an existing process_node for a window is ADOPTED rather than
// duplicated.
func (s *StationService) CreateLoaderBoard(loaderKey string, processID int64) (int64, error) {
	if strings.TrimSpace(loaderKey) == "" {
		return 0, fmt.Errorf("loader_key is required")
	}
	if processID <= 0 {
		return 0, fmt.Errorf("process_id is required: a board belongs to one process on one edge")
	}
	l, err := s.db.GetCoreLoader(loaderKey)
	if err != nil {
		return 0, fmt.Errorf("loader %s: %w", loaderKey, err)
	}
	if l == nil {
		return 0, fmt.Errorf("loader %s: not in this edge's Core loader cache", loaderKey)
	}
	windows := make([]string, 0, len(l.Positions))
	for _, p := range l.Positions {
		if p.PositionNode != "" {
			windows = append(windows, p.PositionNode)
		}
	}
	if len(windows) == 0 {
		return 0, fmt.Errorf("loader %s has no windows to bind: give it members on Core first", loaderKey)
	}

	name := strings.TrimSpace(l.Name)
	if name == "" {
		name = loaderKey
	}
	stationID, err := s.db.CreateOperatorStation(stations.Input{
		ProcessID: processID,
		Name:      name,
		Enabled:   true,
	})
	if err != nil {
		return 0, fmt.Errorf("create board for loader %s: %w", loaderKey, err)
	}
	if err := s.SetNodes(stationID, windows); err != nil {
		// Roll the screen back by hand: SetNodes refuses for reasons the operator
		// can act on (a window Core does not know), and leaving an empty screen
		// behind would be the half-applied state this function exists to avoid.
		if delErr := s.db.DeleteOperatorStation(stationID); delErr != nil {
			return 0, fmt.Errorf("bind windows for loader %s: %w (and rolling back the screen failed: %v)", loaderKey, err, delErr)
		}
		return 0, fmt.Errorf("bind windows for loader %s: %w", loaderKey, err)
	}
	return stationID, nil
}
