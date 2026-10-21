package messaging

import (
	"fmt"
	"log"
	"sort"

	"shingo/protocol"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// bins_feed_groups.go — the bins snapshot's group lines: one per outbound
// group, with levels, of a consume loader with a watched window. What the
// group wants and what each cart at the loader's windows is asked to go back
// as come from one function (engine.UnloaderGroupLines, wired with
// SetGroupLinesFunc); this file finds the groups and hands it their windows.
//
// KEYED ON LEVELS. A group is read for its line only while it is known to
// declare a level: when the list arrived (ConsumeWindowsAt says so in the same
// statement that finds the windows), or since a level save on it
// (BinsLevelChanged). A plant with no levels makes no statement here. A group
// found to have no levels at a build is not read again until a level save.
//
// EVERY WINDOW OF THE LOADER, WATCHED OR NOT. The walk is the group's: it
// runs over every cart at the loader's windows, so two Edges asking get the
// same line. A watched window is named as the Edge watches it; one it does not
// watch keeps the node's own name. The windows' bins are read in the build's
// one bins read, so an unwatched window costs no statement.

// groupLinesFunc answers one levelled group's line; nil for a group with no
// levels. engine.UnloaderGroupLines.
type groupLinesFunc func(group *nodes.Node, levels []store.MaintainLevel,
	windows []protocol.NodeBinsRow) (*protocol.NodeBinsGroup, error)

// watchGroup is one outbound group of a consume loader with a window on an
// Edge's list: the group, whether it declares a level, and every window of its
// consume loaders that touch the list, in the plant's order.
type watchGroup struct {
	name     string
	id       int64
	levelled bool
	windows  []store.ConsumeWindow
}

// SetGroupLinesFunc installs what answers a levelled group's line. Set once at
// the composition root before messages are consumed. Unset: the snapshot
// carries no groups and no level is read.
func (s *CoreDataService) SetGroupLinesFunc(fn func(group *nodes.Node, levels []store.MaintainLevel,
	windows []protocol.NodeBinsRow) (*protocol.NodeBinsGroup, error)) {
	s.lines = fn
}

// groupWindow reports whether a node is a window of one of this list's
// groups. Called under the feed's lock.
func (w *binWatch) groupWindow(nodeID int64) bool {
	for _, g := range w.groups {
		for _, win := range g.windows {
			if win.NodeID == nodeID {
				return true
			}
		}
	}
	return false
}

// indexBinWatchGroups finds the outbound groups of the consume loaders with a
// window on a list that just arrived: its names resolved (one statement), then
// the loaders' windows by group (one statement). A failed read is logged and
// leaves the groups out until the list arrives again; the rows are unaffected.
func (s *CoreDataService) indexBinWatchGroups(station string, list *protocol.BinWatchList) {
	names, digest, ok := s.bins.list(station)
	if !ok || digest != list.Digest {
		return
	}
	resolved, err := s.db.GetNodesByDotNames(names)
	if err != nil {
		log.Printf("core_feeds: bins watch for %s: resolve names: %v — groups left out", station, err)
		return
	}
	ids := make([]int64, 0, len(resolved))
	for _, n := range resolved {
		ids = append(ids, n.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	windows, err := s.db.ConsumeWindowsAt(ids)
	if err != nil {
		log.Printf("core_feeds: bins watch for %s: consume windows: %v — groups left out", station, err)
		return
	}
	s.bins.setGroups(station, digest, windows)
}

// setGroups records the groups found for the list with this digest; their
// names join the names an order is matched on.
func (b *binsFeed) setGroups(station, digest string, windows []store.ConsumeWindow) {
	var groups []watchGroup
	for _, win := range windows {
		if n := len(groups); n == 0 || groups[n-1].name != win.Group {
			groups = append(groups, watchGroup{name: win.Group, id: win.GroupID, levelled: win.Maintained})
		}
		g := &groups[len(groups)-1]
		g.windows = append(g.windows, win)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	w := b.lists[station]
	if w == nil || w.digest != digest {
		return
	}
	w.groups = groups
	for _, g := range groups {
		w.nodeNames[g.name] = true
	}
}

// levelledGroups is the list's groups known to declare a level, none when no
// line function is wired.
func (s *CoreDataService) levelledGroups(station, digest string) []watchGroup {
	if s.lines == nil {
		return nil
	}
	s.bins.mu.Lock()
	defer s.bins.mu.Unlock()
	w := s.bins.lists[station]
	if w == nil || w.digest != digest {
		return nil
	}
	var out []watchGroup
	for _, g := range w.groups {
		if g.levelled && g.id != 0 {
			out = append(out, g)
		}
	}
	return out
}

// setLevelled marks a group of a list as declaring a level or not.
func (b *binsFeed) setLevelled(station, digest string, groupID int64, levelled bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	w := b.lists[station]
	if w == nil || w.digest != digest {
		return
	}
	for i := range w.groups {
		if w.groups[i].id == groupID {
			w.groups[i].levelled = levelled
		}
	}
}

// levelChanged marks a group as levelled on every list that has it, and
// returns those Edges, sorted.
func (b *binsFeed) levelChanged(groupID int64) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for station, w := range b.lists {
		for i := range w.groups {
			if w.groups[i].id == groupID {
				w.groups[i].levelled = true
				out = append(out, station)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// BinsLevelChanged is called after a group's level or layer wants are saved
// or removed. The Edges with a consume loader feeding that group on their list
// are pushed their snapshot, the group now read for its line.
func (s *CoreDataService) BinsLevelChanged(groupID int64) {
	s.bins.markDirty(s.bins.levelChanged(groupID), s.pushBins)
}

func groupNames(groups []watchGroup) []string {
	out := make([]string, len(groups))
	for i, g := range groups {
		out[i] = g.name
	}
	return out
}

// binsReadIDs is every node the build reads bins at: the watched names that
// resolved and the levelled groups' windows, each once, sorted.
func binsReadIDs(names []string, resolved map[string]*nodes.Node, groups []watchGroup) []int64 {
	seen := map[int64]bool{}
	for _, name := range names {
		if n := resolved[name]; n != nil {
			seen[n.ID] = true
		}
	}
	for _, g := range groups {
		for _, win := range g.windows {
			seen[win.NodeID] = true
		}
	}
	ids := make([]int64, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// groupLines answers each levelled group's line: its levels (one statement)
// and the line function's reads. A group that no longer resolves, or has no
// levels, has no line. A failed read fails the build: never a digest over a
// line nobody read.
func (s *CoreDataService) groupLines(station, digest string, groups []watchGroup, names []string,
	resolved map[string]*nodes.Node, first map[int64]*bins.Bin) ([]protocol.NodeBinsGroup, error) {
	out := []protocol.NodeBinsGroup{}
	watched := make(map[int64]string, len(names))
	for _, name := range names {
		if n := resolved[name]; n != nil {
			watched[n.ID] = name
		}
	}
	for _, g := range groups {
		node := resolved[g.name]
		if node == nil {
			continue
		}
		levels, err := s.db.ListMaintainLevels(node.ID)
		if err != nil {
			return nil, fmt.Errorf("bins: levels of %s: %w", g.name, err)
		}
		if len(levels) == 0 {
			s.bins.setLevelled(station, digest, g.id, false)
			continue
		}
		rows := make([]protocol.NodeBinsRow, 0, len(g.windows))
		for _, win := range g.windows {
			name, ok := watched[win.NodeID]
			if !ok {
				name = win.NodeName
			}
			rows = append(rows, binRow(name, first[win.NodeID]))
		}
		line, err := s.lines(node, levels, rows)
		if err != nil {
			return nil, fmt.Errorf("bins: line of %s: %w", g.name, err)
		}
		if line != nil {
			out = append(out, *line)
		}
	}
	return out, nil
}
