package reuse

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// entryVersion is the store format. An entry of any other version is never
// served.
const entryVersion = 1

// keepPerKey is how many entries one key directory keeps (newest first).
const keepPerKey = 4

// Origin is where an entry's verdict was earned.
type Origin struct {
	Tier    string  `json:"tier"`
	Run     string  `json:"run"`
	SHA     string  `json:"sha"`
	Package string  `json:"package"`
	Start   string  `json:"start"`
	WallS   float64 `json:"wall_s"`
}

// Entry is one stored passing run: what it was keyed on, what it touched, and
// the bytes it printed.
type Entry struct {
	Version int    `json:"version"`
	Key     Key    `json:"key"`
	KeyID   string `json:"key_id"`
	// Paths is every path the run touched (test log and trace), with its state
	// at the run's end.
	Paths []PathRec `json:"paths"`
	// Getenv is the names the test log shows read; their values are in Key.Env.
	Getenv []string `json:"getenv"`
	// Processes is how many processes the binary started (-1: unknown), and
	// ChildHow what counted them.
	Processes   int    `json:"processes"`
	SilentForks int    `json:"silent_forks,omitempty"`
	ChildHow    string `json:"child_how"`
	TestMain    bool   `json:"testmain"`
	Traced      bool   `json:"traced"`
	// WholeTree is set when the run's own reads are not the whole of its
	// inputs; Tree is then the tree's hash at the run's end.
	WholeTree    bool   `json:"whole_tree"`
	WholeTreeWhy string `json:"whole_tree_why,omitempty"`
	Root         string `json:"root"`
	Tree         string `json:"tree,omitempty"`
	Origin       Origin `json:"origin"`
	// Output is the binary's stdout and stderr as one stream, byte for byte.
	Output    []byte `json:"output"`
	OutputSHA string `json:"output_sha256"`
	// Sum is the sha256 of the entry's JSON with Sum empty.
	Sum string `json:"sum"`
}

func (e *Entry) seal() error {
	sum := sha256.Sum256(e.Output)
	e.OutputSHA = hex.EncodeToString(sum[:])
	e.Sum = ""
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	s := sha256.Sum256(b)
	e.Sum = hex.EncodeToString(s[:])
	return nil
}

// verify is nil only for an entry that is whole and as written.
func (e *Entry) verify() error {
	if e.Version != entryVersion {
		return fmt.Errorf("version %d, want %d", e.Version, entryVersion)
	}
	if e.KeyID == "" || e.KeyID != e.Key.ID() {
		return fmt.Errorf("key id does not match its key")
	}
	sum := sha256.Sum256(e.Output)
	if hex.EncodeToString(sum[:]) != e.OutputSHA {
		return fmt.Errorf("output does not match its sha256")
	}
	want := e.Sum
	c := *e
	c.Sum = ""
	b, err := json.Marshal(&c)
	if err != nil {
		return err
	}
	s := sha256.Sum256(b)
	if want == "" || hex.EncodeToString(s[:]) != want {
		return fmt.Errorf("entry does not match its sum")
	}
	return nil
}

// writeEntry stores e under its key: to a dot-named temp file in the key's
// directory, synced, then renamed, so a reader sees a whole entry or none.
// It keeps the newest keepPerKey entries of that key.
func writeEntry(store string, e *Entry) (string, error) {
	if err := e.seal(); err != nil {
		return "", err
	}
	dir := filepath.Join(store, e.KeyID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	b, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("%020d-%s.json", time.Now().UnixNano(), e.Sum[:16])
	tmp := filepath.Join(dir, "."+name+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", err
	}
	_, werr := f.Write(b)
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	final := filepath.Join(dir, name)
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	prune(dir)
	return final, nil
}

// entryFiles is a key directory's entries, newest first. Dot names (a write
// in progress or torn) are not entries.
func entryFiles(store, keyID string) []string {
	dir := filepath.Join(store, keyID)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		if e.Type().IsRegular() && !strings.HasPrefix(n, ".") && strings.HasSuffix(n, ".json") {
			out = append(out, filepath.Join(dir, n))
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out
}

// prune removes all but the newest keepPerKey entries; errors are ignored (a
// reader may hold one open on Windows; the next write tries again).
func prune(dir string) {
	files := entryFiles(filepath.Dir(dir), filepath.Base(dir))
	for _, f := range files[min(len(files), keepPerKey):] {
		_ = os.Remove(f)
	}
}

// loadEntry reads and verifies one entry; any error means it is never served.
func loadEntry(path string) (*Entry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var e Entry
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, err
	}
	if err := e.verify(); err != nil {
		return nil, err
	}
	return &e, nil
}
