package reuse

import (
	"fmt"
	"path/filepath"
)

// now re-reads the disk for one lookup, each path and the tree at most once.
type now struct {
	states map[string]string
	trees  map[string]string
}

func newNow() *now { return &now{states: map[string]string{}, trees: map[string]string{}} }

func (n *now) state(p, op string) string {
	k := op + "\x00" + p
	if s, ok := n.states[k]; ok {
		return s
	}
	s, err := stateOf(p, op)
	if err != nil {
		s = "unreadable: " + err.Error() // never equal to a stored state
	}
	n.states[k] = s
	return s
}

func (n *now) tree(root string) string {
	if s, ok := n.trees[root]; ok {
		return s
	}
	ts, err := hashTree(root, timeZero)
	s := ts.Hash
	if err != nil {
		s = "unreadable: " + err.Error()
	}
	n.trees[root] = s
	return s
}

// lookupResult is what the store holds for one key.
type lookupResult struct {
	Hit  *Entry
	File string
	// Miss is why the newest verified entry was not served ("" on a hit).
	Miss string
	// Entries is how many entry files the key had; Unverifiable how many of
	// them failed to load or verify (never served).
	Entries, Unverifiable int
}

// lookup finds the newest stored entry for k whose every touched path, and
// whose tree when it is keyed on the tree, is the same now, by content.
func lookup(store string, k Key, n *now) lookupResult {
	files := entryFiles(store, k.ID())
	r := lookupResult{Entries: len(files)}
	if len(files) == 0 {
		r.Miss = "no entry for this binary, argv, cwd, env, identity and wrapper"
		return r
	}
	for _, f := range files {
		e, err := loadEntry(f)
		if err != nil {
			r.Unverifiable++
			continue
		}
		miss := entryMiss(e, n)
		if miss == "" {
			r.Hit, r.File, r.Miss = e, f, ""
			return r
		}
		if r.Miss == "" {
			r.Miss = miss
		}
	}
	if r.Miss == "" {
		r.Miss = fmt.Sprintf("all %d entries unverifiable", r.Unverifiable)
	}
	return r
}

// entryMiss is the first input of e that is not the same now, or "".
func entryMiss(e *Entry, n *now) string {
	for _, p := range e.Paths {
		if s := n.state(p.Path, p.Op); s != p.State {
			who := "read"
			if p.Child {
				who = "a child read"
			}
			return fmt.Sprintf("content: %s (%s %s)", filepath.ToSlash(p.Path), who, p.Op)
		}
	}
	if e.WholeTree {
		if t := n.tree(e.Root); t != e.Tree {
			return "tree changed (whole-tree key: " + e.WholeTreeWhy + ")"
		}
	}
	return ""
}
