package reuse

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"runtime"
	"slices"
	"sort"
	"strings"
)

// Key is what must be equal before a stored entry is looked at at all. Its ID
// names the store directory the entries live in.
type Key struct {
	// Binary is the sha256 of the test binary's bytes: the Go version, the build
	// tags, every compiled and embedded input and the link flags are in it.
	Binary string `json:"binary_sha256"`
	// Argv is the binary's arguments as go test gave them, less any
	// -test.testlogfile flag (a temp path, new every run).
	Argv []string `json:"argv"`
	// Cwd is the directory go test ran the binary in: its package directory.
	Cwd string `json:"cwd"`
	// Env is the whole environment the binary receives, NAME=VALUE, sorted,
	// with the run-scoped values masked (maskedEnv). The whole block and not
	// only the names the test log shows: a read before m.Run and a read through
	// os.Environ are not logged.
	Env []string `json:"env"`
	// Identity is the sha256 of the identity file's bytes ($GATETIME_IDENTITY):
	// the Postgres image id and server flags, the tool versions, the gate's own
	// scripts, as the gate writes them.
	Identity string `json:"identity_sha256"`
	// Wrapper is the sha256 of the program running this code.
	Wrapper string `json:"wrapper_sha256"`
}

// ID is the hex sha256 of the key's canonical JSON.
func (k Key) ID() string {
	b, err := json.Marshal(k)
	if err != nil {
		// A struct of strings always marshals; an empty ID matches nothing.
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// strippedEnv is whether a variable is removed from the binary's environment,
// and so from the key. Stripped, never masked: the binary cannot read it, nor
// can anything it starts, so "no test reads it" holds by construction, and a
// variable set in one gate and absent in the next cannot key them apart.
//
// The GATETIME_ and GATE_REUSE names are this wrapper's own switches.
//
// GATE_TIER is the gate's tier: removed so that dev, push and cert hand one
// binary one environment (cert audits push's entries). One test reads it:
// shingo-core/internal/schemadump's releaseCacheForTests (snapshot_test.go)
// rebuilds every old release when it is cert. So under this wrapper (the
// Linux side's dev, push and cert) that test never sees cert and takes its
// release cache as push does; GitHub still rebuilds (GITHUB_ACTIONS). The
// comment this replaces said no Go file read it: true at 20e38817, false since
// 9eb45dd4. Left as it is and reported (brief A2, lane reuse): keying the tier
// would part cert's keys from push's.
//
// perRunEnv is what the terminal and the gate set per session or per run
// (R46, brief A2 step 4).
func strippedEnv(name string) bool {
	n := envName(name)
	return n == "GATE_TIER" || n == "GATE_REUSE" || strings.HasPrefix(n, "GATE_REUSE_") || strings.HasPrefix(n, "GATETIME_") || perRunEnv[n]
}

// perRunEnv are the variables two gates of one unchanged tree keyed apart on,
// 2026-11-24, every other part of their keys equal (the mirror's stored keys
// diffed: work/loc-census-2026-11-20/gate-fast/lanes-A2/reuse): a second
// Windows Terminal tab carries its own WT_SESSION and WT_PROFILE_ID, which WSL
// hands the Linux side through WSLENV; GATE_NO_LOCK is set or not by whoever
// starts the gate, and GATE_LOCK_HELD is there only when the run holds the
// lock; `gatetime run` sets a fresh GATE_TIMING_TOKEN, and a run timed with a
// flag of its own has another GATE_TIMING_DIR; GATE_TESTSET_DIR is there only
// with test sets on. No test needs one: of the five modules' Go, only
// gatetime's flagDir reads one (GATE_TIMING_DIR), and every test that reaches
// it sets it itself (flag_test.go, run_test.go); the scripts that read the
// rest (the gate, its lock and timing flag, its pins) are started by no test;
// and none of the names is in the bytes of a program a test starts (go and its
// tools, git and its helpers, node, docker, sh, bash). Pinned by
// TestKeyEnv_ATerminalsAndAGatesPerRunVariablesAreNotInTheKey.
//
// WSLENV stays in the key: WSL's interop reads it whenever a test starts a
// Windows program (it names the variables that cross), and gatetime's host
// reader builds on it. Both Windows Terminal tabs measured had the same list.
var perRunEnv = map[string]bool{
	"WT_SESSION":        true,
	"WT_PROFILE_ID":     true,
	"GATE_NO_LOCK":      true,
	"GATE_LOCK_HELD":    true,
	"GATE_TIMING_TOKEN": true,
	"GATE_TIMING_DIR":   true,
	"GATE_TESTSET_DIR":  true,
}

// maskedEnv are variables the binary receives unchanged whose VALUES are run
// scoped, so the key carries a fixed word for them instead. SHINGO_TEST_PG is a
// fresh port each gate run and the template name is per run; what the server
// is (image, flags) is in the identity. TMPDIR, TMP and TEMP are set by the
// wrapper to a directory made empty for this run. WSL_INTEROP is the interop
// socket WSL makes for each session (/run/WSL/<pid>_interop): every gate a
// Windows worktree runs in its WSL mirror is a new wsl.exe session, so two
// gates of one unchanged mirror keyed apart on it alone and served 0 of 112
// (2026-11-24, pinned by TestKeyEnv_MasksRunScopedValuesAndKeepsTheirNames).
// That interop is there is keyed; which socket is not: a test that starts a
// Windows program needs it.
var maskedEnv = map[string]string{
	"SHINGO_TEST_PG":          "<run-scoped address; the server is in the identity>",
	"SHINGO_TEST_PG_TEMPLATE": "<run-scoped template name>",
	"WSL_INTEROP":             "<this WSL session's interop socket>",
	"TMPDIR":                  "<private temp dir>",
	"TMP":                     "<private temp dir>",
	"TEMP":                    "<private temp dir>",
}

// envName is a variable's name as the OS compares it: Windows names are
// case-insensitive.
func envName(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(name)
	}
	return name
}

// childEnv is the environment the binary runs with: the wrapper's own, less
// the stripped names, with the temp variables pointed at tmp and PATH without
// its repeated entries.
//
// A PATH lookup stops at the first directory that has the name, so a later
// copy of an earlier directory is never the one found: dropping it changes no
// lookup. Two terminals' PATHs differed only so on 2026-11-24 (WSL appends the
// caller's Windows PATH, and the second tab's Git Bash login shell had put its
// own directories in front of it again), and keyed apart on it. A PATH whose
// first occurrences differ, in which directories or in their order, still keys
// apart: there a lookup can find another program.
func childEnv(env []string, tmp string) []string {
	out := make([]string, 0, len(env)+3)
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		switch n := envName(name); {
		case strippedEnv(name), n == "TMPDIR", n == "TMP", n == "TEMP":
			continue
		case n == "PATH":
			kv = name + "=" + firstOccurrences(value)
		}
		out = append(out, kv)
	}
	out = append(out, "TMPDIR="+tmp)
	if runtime.GOOS == "windows" {
		out = append(out, "TMP="+tmp, "TEMP="+tmp)
	}
	return out
}

// firstOccurrences is a PATH-style list with every entry after its first
// occurrence removed, in order.
func firstOccurrences(list string) string {
	sep := string(os.PathListSeparator)
	seen := map[string]bool{}
	var out []string
	for _, e := range strings.Split(list, sep) {
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	return strings.Join(out, sep)
}

// keyEnv is the key's view of the environment the binary runs with. A
// stripped name is never in it, even in an environment childEnv did not make.
func keyEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if strippedEnv(name) {
			continue
		}
		if w, ok := maskedEnv[envName(name)]; ok {
			kv = name + "=" + w
		}
		out = append(out, kv)
	}
	sort.Strings(out)
	return out
}

// keyParts is what in key a differs from key b: the parts other than env, in
// the order the key lists them, and the env names whose value or presence
// differs, sorted.
func keyParts(a, b Key) (parts, env []string) {
	for _, p := range []struct {
		name string
		same bool
	}{
		{"binary", a.Binary == b.Binary},
		{"argv", slices.Equal(a.Argv, b.Argv)},
		{"cwd", a.Cwd == b.Cwd},
		{"identity", a.Identity == b.Identity},
		{"wrapper", a.Wrapper == b.Wrapper},
	} {
		if !p.same {
			parts = append(parts, p.name)
		}
	}
	ea, eb := map[string]string{}, map[string]string{}
	for _, kv := range a.Env {
		n, v, _ := strings.Cut(kv, "=")
		ea[n] = v
	}
	for _, kv := range b.Env {
		n, v, _ := strings.Cut(kv, "=")
		eb[n] = v
	}
	for n, v := range ea {
		if w, ok := eb[n]; !ok || w != v {
			env = append(env, n)
		}
	}
	for n := range eb {
		if _, ok := ea[n]; !ok {
			env = append(env, n)
		}
	}
	sort.Strings(env)
	return parts, env
}

// keyDiff is what in key a moved from key b, in the words the gate prints after
// "the key's" (scripts/gate-reuse.sh, reuse_explain): "env differed in PATH,
// WT_SESSION", "binary differed", "argv and identity differed", "identity
// differed, and its env in X"; "" for one key.
func keyDiff(a, b Key) string {
	parts, env := keyParts(a, b)
	names := strings.Join(env, ", ")
	switch {
	case len(parts) == 0 && len(env) == 0:
		return ""
	case len(parts) == 0:
		return "env differed in " + names
	}
	s := parts[len(parts)-1]
	if len(parts) > 1 {
		s = strings.Join(parts[:len(parts)-1], ", ") + " and " + s
	}
	s += " differed"
	if len(env) > 0 {
		s += ", and its env in " + names
	}
	return s
}

// nearestDiff is keyDiff of k against the one of keys (newest first) it is
// least apart from, counting each part and each env name as one; the newer of
// two as near. "" when keys holds none but k itself.
func nearestDiff(k Key, keys []Key) string {
	best, bestN := -1, 0
	for i, o := range keys {
		parts, env := keyParts(k, o)
		n := len(parts) + len(env)
		if n == 0 {
			continue
		}
		if best < 0 || n < bestN {
			best, bestN = i, n
		}
	}
	if best < 0 {
		return ""
	}
	return keyDiff(k, keys[best])
}

// testlogFlag is the flag go test never passes under -exec and the wrapper
// adds itself (the flag package takes one dash or two).
const testlogFlag = "test.testlogfile="

// keyArgv is argv less any -test.testlogfile flag.
func keyArgv(argv []string) []string {
	out := make([]string, 0, len(argv))
	for _, a := range argv {
		if !strings.HasPrefix(strings.TrimLeft(a, "-"), testlogFlag) {
			out = append(out, a)
		}
	}
	return out
}

// selectors are the test flags that make a run cover less than the package's
// whole test set (or something else than it).
var selectors = []string{"test.run", "test.skip", "test.list", "test.bench", "test.fuzz"}

// filtered is why a run of argv does not run the package's whole test set, or
// "". Such a run's verdict is never stored: a later full run must not be
// served from it, and its argv differs from a full run's anyway.
func filtered(argv []string) string {
	for _, a := range argv {
		flag, v, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		for _, s := range selectors {
			// "-test.run X" (value in the next argument) counts whatever X is.
			if flag == s && (!hasValue || v != "") {
				return "filtered run (" + a + ")"
			}
		}
	}
	return ""
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
