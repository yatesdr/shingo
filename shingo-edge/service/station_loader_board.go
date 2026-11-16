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
// WHAT IS TAKEN WITH NO PERSON, AND WHY NO MORE. Core sends every loader to
// every edge (BuildLoaderInfos takes no edge argument), so for a long time the
// only thing assigning a loader to an edge was a person putting its windows on
// one of that edge's screens, and this file made no board on its own. Core now
// says which Edge works a loader (bin_loaders.edge_station_uid), and the take
// (station_loader_take.go) acts on that:
//
//   - Core names THIS Edge: the Edge makes the loader's process and screen
//     through CreateLoaderBoard below when it has none, and keeps the windows
//     Core adds and removes on and off it.
//   - Core names ANOTHER Edge: nothing is made or offered here (no gap below,
//     and CreateLoaderBoard refuses), and a screen of it from before loses the
//     loader's windows but stays.
//   - Core names NONE: a window Core adds goes onto the one screen already
//     holding the loader's other windows; with none, the button below keeps
//     the person answering "which process", as before.
//
// What is still never taken: a screen with any position no loader holds is a
// person's (SPR's Unloader windows sit on two press screens), so the Edge does
// not write it, and does not make a second screen for a loader whose windows
// already sit on one. Those stay the person's decision because the plant
// made them that way, and nothing Core sends says otherwise.

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
// is the whole design. Partial binding is NORMAL, not a gap: a loader may have
// windows that no screen here works. Reporting "unbound windows" would put a
// permanent complaint on a correctly configured loader — the same failure mode
// as a threshold warning that fires on every loader that does not use thresholds.
//
// ITS WINDOWS ARE WHAT A BOARD WORKS AS TILES (CoreLoader.BoardWindows, the one
// rule): a dedicated loader's buffer spots are not among them. The board draws
// those from the loader (the buffer strip), so they are never bound and never
// counted (SPR's Bin Loader: 22 homes on its screen, 9 buffer spots on none).
//
// A loader with no windows at all is skipped too, a loader of buffer spots
// only among them. There is nothing to bind, and Core's own box already says
// so (configGapHtml) at the place it can be fixed.
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
		windows := l.BoardWindows()
		if len(windows) == 0 {
			continue
		}
		// A loader Core gives another Edge is that Edge's to board.
		if f, ok := s.loaderFact(l.LoaderKey); ok && f.Case == LoaderCaseElsewhere {
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
// windows to it, in that order, as one action. The screen names the loader
// (core_loader_key). "Make a screen" calls it, and so does the take when Core
// names this Edge; it refuses a loader Core names another Edge for.
//
// ITS WINDOWS, NOT ITS BUFFER SPOTS (CoreLoader.BoardWindows, the rule "Core
// added" follows too; owner, 2026-11-16: "it shouldnt put its buffer spots on
// the screen"). The board draws a dedicated loader's buffer spots from the
// loader; one bound here as well was a second tile with no part, whose sheet
// offers only CLOSE. A loader of buffer spots only has nothing to bind.
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
	windows := l.BoardWindows()
	if len(windows) == 0 {
		return 0, fmt.Errorf("loader %s has no windows to bind: give it members on Core first (a buffer spot is drawn from the loader, never bound)", loaderKey)
	}
	if f, ok := s.loaderFact(loaderKey); ok && f.Case == LoaderCaseElsewhere {
		return 0, fmt.Errorf("loader %s: Core names %s to work it, and that Edge makes its screen (set it on Core, under Loaders)", loaderKey, edgeName(f))
	}

	name := strings.TrimSpace(l.Name)
	if name == "" {
		name = loaderKey
	}
	stationID, err := s.db.CreateOperatorStation(stations.Input{
		ProcessID: processID,
		Name:      name,
		Enabled:   true,
		// The screen is this loader's from here on, by its key: a later
		// take finds it even after every window has come off it.
		CoreLoaderKey: loaderKey,
	})
	if err != nil {
		return 0, fmt.Errorf("create board for loader %s: %w", loaderKey, err)
	}
	if _, err := s.SetNodes(stationID, windows); err != nil {
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

// edgeName is how a page names the Edge Core picked: its display name, else
// its station uid.
func edgeName(f LoaderFacts) string {
	if strings.TrimSpace(f.EdgeName) != "" {
		return f.EdgeName
	}
	return f.EdgeUID
}
