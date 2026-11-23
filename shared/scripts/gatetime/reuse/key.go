package reuse

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"runtime"
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

// strippedEnv is whether a variable is removed from the binary's environment
// and so from the key. GATE_TIER is the gate's tier name: removed so that dev,
// push and cert hand one binary one environment (cert audits push's entries).
// No Go file in the repo reads it (git grep, 20e38817). The GATETIME_ and
// GATE_REUSE names are this wrapper's own switches.
func strippedEnv(name string) bool {
	n := envName(name)
	return n == "GATE_TIER" || n == "GATE_REUSE" || strings.HasPrefix(n, "GATE_REUSE_") || strings.HasPrefix(n, "GATETIME_")
}

// maskedEnv are variables the binary receives unchanged whose VALUES are run
// scoped, so the key carries a fixed word for them instead. SHINGO_TEST_PG is a
// fresh port each gate run and the template name is per run; what the server
// is (image, flags) is in the identity. TMPDIR, TMP and TEMP are set by the
// wrapper to a directory made empty for this run. GATE_LOCK_HELD is the gate
// lock's token: its presence is keyed, its value is not.
var maskedEnv = map[string]string{
	"SHINGO_TEST_PG":          "<run-scoped address; the server is in the identity>",
	"SHINGO_TEST_PG_TEMPLATE": "<run-scoped template name>",
	"GATE_LOCK_HELD":          "<lock token>",
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
// the stripped names, with the temp variables pointed at tmp.
func childEnv(env []string, tmp string) []string {
	out := make([]string, 0, len(env)+3)
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		switch n := envName(name); {
		case strippedEnv(name), n == "TMPDIR", n == "TMP", n == "TEMP":
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "TMPDIR="+tmp)
	if runtime.GOOS == "windows" {
		out = append(out, "TMP="+tmp, "TEMP="+tmp)
	}
	return out
}

// keyEnv is the key's view of the environment the binary runs with.
func keyEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if w, ok := maskedEnv[envName(name)]; ok {
			kv = name + "=" + w
		}
		out = append(out, kv)
	}
	sort.Strings(out)
	return out
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
