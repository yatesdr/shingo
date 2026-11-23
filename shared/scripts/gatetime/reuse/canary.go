package reuse

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// canaryPkg is the canary's package directory under the repo root.
const canaryPkg = "shared/scripts/gatetime/reuse/canary"

// CanaryMain runs the canary package through this program's `exec` in record
// mode, in a store of its own, and checks the record against what the canary
// is known to do (canary/canary_test.go). It prints one line, "ok   reuse
// canary (...)" or "FAIL reuse canary: ...", and exits 0 or 1. The gate turns
// reuse off for its run unless this exits 0.
//
//	canary -root DIR [-identity FILE]
func CanaryMain(args []string) int {
	fs := flag.NewFlagSet("canary", flag.ContinueOnError)
	root := fs.String("root", "", "the repo's working tree")
	identity := fs.String("identity", "", "the identity file (default: a temp one)")
	if err := fs.Parse(args); err != nil || *root == "" {
		fmt.Fprintln(os.Stderr, "usage: canary -root DIR [-identity FILE]")
		return 2
	}
	why, how := runCanary(*root, *identity)
	if why != "" {
		fmt.Println("FAIL reuse canary: " + why)
		return 1
	}
	fmt.Println("ok   reuse canary (" + how + ")")
	return 0
}

func runCanary(root, identity string) (why, how string) {
	self, err := os.Executable()
	if err != nil {
		return "no executable: " + err.Error(), ""
	}
	work, err := os.MkdirTemp("", "gatetime-canary-*")
	if err != nil {
		return err.Error(), ""
	}
	defer os.RemoveAll(work)
	if identity == "" {
		identity = filepath.Join(work, "identity")
		if err := os.WriteFile(identity, []byte("canary\n"), 0o644); err != nil {
			return err.Error(), ""
		}
	}
	nonce := make([]byte, 8)
	_, _ = rand.Read(nonce)
	value := hex.EncodeToString(nonce)
	store, log := filepath.Join(work, "store"), filepath.Join(work, "log")
	cmd := exec.Command("go", "test", "-count=1", "-exec", "'"+self+"' exec", "./scripts/gatetime/reuse/canary")
	cmd.Dir = filepath.Join(root, "shared")
	cmd.Env = append(os.Environ(), "GATETIME_REUSE="+ModeRecord, "GATETIME_STORE="+store, "GATETIME_REUSE_LOG="+log,
		"GATETIME_IDENTITY="+identity, "SHINGO_REUSE_CANARY="+value)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Sprintf("go test of the canary failed: %v: %s", err, strings.TrimSpace(string(out))), ""
	}
	d, err := lastDecision(log)
	if err != nil {
		return err.Error(), ""
	}
	if d.Decision != "run" || d.Exit != 0 || !d.Stored {
		return fmt.Sprintf("the canary's run was not stored (decision %s, exit %d, refused %q)", d.Decision, d.Exit, d.Refused), ""
	}
	files := entryFiles(store, d.KeyID)
	if len(files) == 0 {
		return "no stored entry for the canary's key", ""
	}
	e, err := loadEntry(files[0])
	if err != nil {
		return "the canary's entry does not verify: " + err.Error(), ""
	}
	return checkCanary(e, filepath.Join(root, filepath.FromSlash(canaryPkg)), root, value)
}

func lastDecision(log string) (Decision, error) {
	b, err := os.ReadFile(log)
	if err != nil {
		return Decision{}, fmt.Errorf("no decision log: %v", err)
	}
	var d Decision
	n := 0
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if err := json.Unmarshal(sc.Bytes(), &d); err != nil {
			return Decision{}, fmt.Errorf("decision log: %v", err)
		}
		n++
	}
	if n != 1 {
		return Decision{}, fmt.Errorf("decision log has %d lines, want 1 (one binary)", n)
	}
	return d, nil
}

// checkCanary compares the entry with what the canary does. Every difference
// is listed; any one turns reuse off.
func checkCanary(e *Entry, pkg, root, value string) (why, how string) {
	var bad []string
	want := func(p, op string, child bool) {
		for _, r := range e.Paths {
			if r.Path == p && r.Op == op && r.Child == child {
				if now, err := stateOf(p, op); err != nil || now != r.State {
					bad = append(bad, fmt.Sprintf("%s recorded as %q, is %q now", filepath.ToSlash(p), r.State, now))
				}
				return
			}
		}
		who := "the binary"
		if child {
			who = "a child"
		}
		bad = append(bad, fmt.Sprintf("no %s of %s by %s", op, filepath.ToSlash(p), who))
	}
	want(filepath.Join(pkg, "testdata", "in.txt"), OpOpen, false)
	want(filepath.Join(root, "go.work"), OpOpen, false)
	want(filepath.Join(pkg, "testdata"), OpOpen, false)
	if !contains(e.Getenv, "SHINGO_REUSE_CANARY") || !contains(e.Key.Env, "SHINGO_REUSE_CANARY="+value) {
		bad = append(bad, "the env read of SHINGO_REUSE_CANARY is not in the record")
	}
	// One or more: the canary starts one child, which runs a program (the test
	// binary again). On Linux the tracer also sees go1.25's clone-pidfd probe
	// fork and counts it apart, as a silent fork (trace_linux_amd64.go, result).
	if e.Processes < 1 || (e.ChildHow != "ptrace" && e.ChildHow != "job") {
		bad = append(bad, fmt.Sprintf("children: %d by %q, want 1 or more by ptrace or job", e.Processes, e.ChildHow))
	}
	if !e.WholeTree || e.Tree == "" {
		bad = append(bad, "a binary that started a child is not keyed on the whole tree")
	}
	how = fmt.Sprintf("env, a file in and out of its module, a listing, %d process(es) started, counted by %s", e.Processes, e.ChildHow)
	if e.Traced {
		want(filepath.Join(pkg, "testdata", "pre.txt"), OpOpen, false)
		want(filepath.Join(pkg, "testdata", "child.txt"), OpOpen, true)
		how += "; the trace saw the package-var read and the child's read"
	} else {
		how += "; untraced: a package-var read and a child's reads are not seen here"
	}
	if len(bad) > 0 {
		return strings.Join(bad, "; "), ""
	}
	return "", how
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
