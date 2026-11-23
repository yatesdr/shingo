package reuse

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// treeState is the whole working tree as one hash: every path
// `git ls-files -co --exclude-standard` lists (tracked, and untracked but not
// ignored), with its state read from disk now (stateOf, op open). Git lists the
// paths; it is never asked for content, so the index, a commit or a racy
// stat cache cannot stand in for the bytes. A tracked path deleted on disk is
// "missing". changed names paths whose change time is at or after since less
// 2 s (empty when since is zero).
//
// It is the key of a binary whose reads the wrapper cannot see whole: one that
// started a child, has a TestMain, or ran untraced.
type treeState struct {
	Hash    string
	Changed []string
	Files   int
}

// gitEnv is the wrapper's environment without the variables that would point
// git at another repository or index than the one at root.
func gitEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			out = append(out, kv)
		}
	}
	return out
}

// repoRoot is the top of the working tree holding dir, or "".
func repoRoot(dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--show-toplevel")
	cmd.Env = gitEnv()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return filepath.Clean(strings.TrimSpace(string(out)))
}

func hashTree(root string, since time.Time) (treeState, error) {
	if root == "" {
		return treeState{}, fmt.Errorf("no working tree")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", root, "ls-files", "-z", "-co", "--exclude-standard")
	cmd.Env = gitEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return treeState{}, fmt.Errorf("git ls-files: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	names := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if len(names) == 1 && names[0] == "" {
		names = nil
	}
	sort.Strings(names)
	names = dedupe(names) // a conflicted path is listed once per stage
	states := make([]string, len(names))
	changed := make([]bool, len(names))
	errs := make([]error, len(names))
	var wg sync.WaitGroup
	next := make(chan int)
	for w := 0; w < max(2, runtime.NumCPU()); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				p := filepath.Join(root, filepath.FromSlash(names[i]))
				states[i], errs[i] = stateOf(p, OpOpen)
				changed[i] = !since.IsZero() && changedSince(p, OpOpen, since)
			}
		}()
	}
	for i := range names {
		next <- i
	}
	close(next)
	wg.Wait()
	h := sha256.New()
	ts := treeState{Files: len(names)}
	for i, n := range names {
		if errs[i] != nil {
			return treeState{}, fmt.Errorf("%s: %v", n, errs[i])
		}
		fmt.Fprintf(h, "%s\x00%s\n", n, states[i])
		if changed[i] {
			ts.Changed = append(ts.Changed, n)
		}
	}
	ts.Hash = hex.EncodeToString(h.Sum(nil))
	return ts, nil
}

func dedupe(sorted []string) []string {
	out := sorted[:0]
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			out = append(out, s)
		}
	}
	return out
}
