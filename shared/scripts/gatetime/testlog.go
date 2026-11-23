package main

import (
	"bytes"
	"path/filepath"
	"sort"
	"strings"
)

// testlogMagic opens every test log the testing package writes
// (testing/internal/testdeps, StartTestLog); cmd/go checks for it too.
var testlogMagic = []byte("# test log\n")

// testlogReads is what one test log says the binary read: env names, and
// paths with the operations that touched them (open, stat, chdir), each path
// absolute and cleaned. state is "complete"; "absent" (no log to read: none was
// asked for, or the wrapper could not read it back); "empty" (the binary wrote
// none: it exited before m.Run started its log, a TestMain that never calls
// m.Run included); "truncated" (no final newline: the binary died mid-write;
// the torn line is dropped) or "malformed".
type testlogReads struct {
	state string
	env   []string
	paths map[string][]string
}

// parseTestlog reads a test log as cmd/go's computeTestInputsID does
// (cmd/go/internal/test): one "op name" per line; a relative name is resolved
// against the directory of the last chdir (always absolute), starting at the
// binary's working directory. Unlike Go's cache, nothing is dropped for lying
// outside the module: every path is kept.
func parseTestlog(data []byte, cwd string) testlogReads {
	r := testlogReads{paths: map[string][]string{}}
	switch {
	case data == nil:
		r.state = "absent"
		return r
	case len(data) == 0:
		r.state = "empty"
		return r
	case !bytes.HasPrefix(data, testlogMagic):
		r.state = "malformed"
		return r
	case data[len(data)-1] != '\n':
		r.state = "truncated"
	default:
		r.state = "complete"
	}
	env := map[string]bool{}
	ops := map[string]map[string]bool{}
	add := func(op, p string) {
		if ops[p] == nil {
			ops[p] = map[string]bool{}
		}
		ops[p][op] = true
	}
	pwd := cwd
	lines := bytes.Split(bytes.TrimPrefix(data, testlogMagic), []byte("\n"))
	if r.state == "truncated" {
		lines = lines[:len(lines)-1] // the torn last line
	}
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		op, name, ok := strings.Cut(string(line), " ")
		if !ok {
			r.state = "malformed"
			continue
		}
		switch op {
		case "getenv":
			env[name] = true
		case "chdir":
			pwd = filepath.Clean(name)
			add("chdir", pwd)
		case "open", "stat":
			if !filepath.IsAbs(name) {
				name = filepath.Join(pwd, name)
			}
			add(op, filepath.Clean(name))
		default:
			r.state = "malformed"
		}
	}
	for n := range env {
		r.env = append(r.env, n)
	}
	sort.Strings(r.env)
	for p, set := range ops {
		for op := range set {
			r.paths[p] = append(r.paths[p], op)
		}
		sort.Strings(r.paths[p])
	}
	return r
}
