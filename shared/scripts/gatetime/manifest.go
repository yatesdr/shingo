package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// manifestSchema is the manifest format's version. A change to what a field
// means bumps it; the format is written out in
// work/loc-census-2026-11-20/gate-fast/lanes/timing/MANIFEST-SCHEMA.md.
const manifestSchema = 1

// Manifest is what one test binary's run read and did, as `gatetime exec`
// observed it. Record only: nothing reads a manifest to decide a verdict.
type Manifest struct {
	Schema int `json:"schema"`
	// ToolSHA256 is the sha256 of the gatetime executable that wrote this.
	ToolSHA256 string `json:"tool_sha256"`
	// Package is the import path: the module path of the nearest go.mod above
	// Cwd, joined with Cwd's place under it (go test runs a binary in its
	// package's directory).
	Package    string `json:"package"`
	ModuleRoot string `json:"module_root"`
	Cwd        string `json:"cwd"`
	// Binary is the path go test handed the wrapper; BinarySHA256 its content.
	Binary       string `json:"binary"`
	BinarySHA256 string `json:"binary_sha256"`
	// Argv is the binary's arguments as go test gave them, without the
	// -test.testlogfile flag the wrapper added (TestlogArg).
	Argv       []string `json:"argv"`
	TestlogArg string   `json:"testlog_arg"`
	// IdentityFile is $GATETIME_IDENTITY; IdentitySHA256 the sha256 of its
	// bytes, "none" when unset, "error: ..." when unreadable.
	IdentityFile   string  `json:"identity_file"`
	IdentitySHA256 string  `json:"identity_sha256"`
	Start          string  `json:"start"` // UTC, RFC 3339 with nanoseconds
	WallS          float64 `json:"wall_s"`
	// CPUS is the user and kernel seconds of the binary and every process it
	// started (the job's accounting), -1 when the job could not be had.
	CPUS float64 `json:"cpu_s"`
	Exit int     `json:"exit"`
	// Children is the processes the binary started, directly or not (the
	// job's TotalProcesses less the binary), -1 when unknown.
	Children int `json:"children"`
	// Job is "ok", or why CPUS and Children are missing.
	Job string `json:"job"`
	// Testlog is the state of the binary's test log (testlogReads.state).
	Testlog string     `json:"testlog"`
	Env     []EnvRead  `json:"env"`
	Paths   []PathRead `json:"paths"`
	Errors  []string   `json:"errors,omitempty"`
}

// EnvRead is one env var the binary read. GODEBUG is always listed: the
// runtime reads it without telling the test log, and Go's own cache keys on it.
type EnvRead struct {
	Name string `json:"name"`
	// Value is "unset", or "sha256:<hex>" of the value in the wrapper's
	// environment, which is the one the binary started with.
	Value string `json:"value"`
}

// PathRead is one path the binary opened, statted or changed into.
type PathRead struct {
	Path string   `json:"path"` // absolute, cleaned, forward slashes
	Ops  []string `json:"ops"`  // of chdir, open, stat
	// Scope is "module" (under ModuleRoot), "temp" (under the OS temp
	// directory: almost always this run's own output) or "outside".
	Scope string `json:"scope"`
	// Hash is the content after the binary exited: "file:<sha256>",
	// "dir:<sha256 of the sorted listing>", "absent", "special:<mode>" for
	// what is neither file nor directory, or "error: ...".
	Hash string `json:"hash"`
	// ChangedInRun is true when the path's mtime is at or after the run's
	// start less 2 s (Go's own test-cache slack): its hash may be this run's
	// output, not its input.
	ChangedInRun bool `json:"changed_in_run,omitempty"`
}

// sha256File is the hex sha256 of a file's bytes.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close() // read-only: a close error loses nothing
	h := sha256.New()
	if _, err := io.Copy(h, bufio.NewReaderSize(f, 1<<20)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashPath is a path's content as PathRead.Hash spells it.
func hashPath(p string, since time.Time) (hash string, changed bool) {
	fi, err := os.Stat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "absent", false
	case err != nil:
		return "error: " + err.Error(), false
	}
	changed = !fi.ModTime().Before(since.Add(-2 * time.Second))
	switch {
	case fi.IsDir():
		ents, err := os.ReadDir(p)
		if err != nil {
			return "error: " + err.Error(), changed
		}
		names := make([]string, 0, len(ents))
		for _, e := range ents {
			n := e.Name()
			if e.IsDir() {
				n += "/"
			}
			names = append(names, n)
		}
		sort.Strings(names)
		sum := sha256.Sum256([]byte(strings.Join(names, "\n")))
		return "dir:" + hex.EncodeToString(sum[:]), changed
	case fi.Mode().IsRegular():
		s, err := sha256File(p)
		if err != nil {
			return "error: " + err.Error(), changed
		}
		return "file:" + s, changed
	default:
		return "special:" + fi.Mode().String(), changed
	}
}

// inDir is whether p lies at or under dir. filepath.Rel compares Windows
// paths case-insensitively, as the file system does.
func inDir(p, dir string) bool {
	if dir == "" {
		return false
	}
	rel, err := filepath.Rel(dir, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// modulePackage finds the nearest go.mod at or above dir and the import path
// dir has under it. Both are "" when no go.mod is found.
func modulePackage(dir string) (root, pkg string) {
	for d := dir; ; d = filepath.Dir(d) {
		data, err := os.ReadFile(filepath.Join(d, "go.mod"))
		if err == nil {
			mod := ""
			for _, line := range strings.Split(string(data), "\n") {
				if f := strings.Fields(line); len(f) >= 2 && f[0] == "module" {
					mod = strings.Trim(f[1], `"`)
					break
				}
			}
			rel, err := filepath.Rel(d, dir)
			if err != nil || mod == "" {
				return d, ""
			}
			if rel == "." {
				return d, mod
			}
			return d, mod + "/" + filepath.ToSlash(rel)
		}
		if filepath.Dir(d) == d {
			return "", ""
		}
	}
}

// envValue is EnvRead.Value for one name in this process's environment.
func envValue(name string) string {
	v, ok := os.LookupEnv(name)
	if !ok {
		return "unset"
	}
	sum := sha256.Sum256([]byte(v))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// fillReads turns a parsed test log into the manifest's Env and Paths.
func (m *Manifest) fillReads(r testlogReads, start time.Time) {
	m.Testlog = r.state
	names := map[string]bool{"GODEBUG": true}
	for _, n := range r.env {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	m.Env = make([]EnvRead, 0, len(sorted))
	for _, n := range sorted {
		m.Env = append(m.Env, EnvRead{Name: n, Value: envValue(n)})
	}
	paths := make([]string, 0, len(r.paths))
	for p := range r.paths {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	// The temp directory in both spellings: on Windows TMP is often an 8.3
	// short name (C:\Users\ABCDEF~1\...) while EvalSymlinks gives the long one,
	// and a test may hold either.
	tmp := os.TempDir()
	longTmp, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		longTmp = tmp
	}
	root := filepath.FromSlash(m.ModuleRoot)
	m.Paths = make([]PathRead, 0, len(paths))
	for _, p := range paths {
		scope := "outside"
		switch {
		case inDir(p, root):
			scope = "module"
		case inDir(p, tmp) || inDir(p, longTmp):
			scope = "temp"
		}
		h, changed := hashPath(p, start)
		m.Paths = append(m.Paths, PathRead{Path: filepath.ToSlash(p), Ops: r.paths[p], Scope: scope, Hash: h, ChangedInRun: changed})
	}
}

// writeManifest writes m into dir as one JSON file, by rename so that a reader
// never sees half of one.
func writeManifest(dir string, m *Manifest) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return "", err
	}
	safe := strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(m.Package)
	if safe == "" {
		safe = "unknown"
	}
	name := fmt.Sprintf("%d-%s-%d.json", time.Now().UnixNano(), safe, os.Getpid())
	tmp := filepath.Join(dir, "."+name+".tmp")
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return "", err
	}
	final := filepath.Join(dir, name)
	return final, os.Rename(tmp, final)
}

// readManifest reads one manifest file.
func readManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &m, nil
}
