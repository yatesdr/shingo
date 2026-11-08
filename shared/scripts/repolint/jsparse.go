package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// --- js-parse --------------------------------------------------------------
//
// Every tracked .js file parses, as an ES module or as CommonJS.
//
// WHY. Go tests never parse JavaScript: the www drift tests scan for PATTERNS,
// and none of them is a parser, so a hard syntax error in a shipped bundle
// passes every other gate and reaches a plant. It did. Hopkinsville 2026-09-08:
// a stray } left behind by a deletion in operator-release.js took every
// operator HMI dark. The operator station is an ES MODULE GRAPH, so one
// unparseable file stops the graph linking, operator.js never runs a line, and
// every board sits on "Loading..." forever while the server stays healthy.
//
// ESM FIRST, THEN COMMONJS. The two are not the same grammar: ESM is implicitly
// strict, so a legitimately classic script (an octal literal, with, duplicate
// params) would be a false positive under ESM alone. Trying CommonJS after gives
// classic scripts an honest pass, and gives nothing up: the failure this guards
// against, an unbalanced brace, fails both.
//
// ONE NODE PROCESS. The guard copied each file to a .mjs and then a .cjs and ran
// node --check on each, about five processes a file. Here one node compiles
// every file without running it: as a module through the vm module's source
// text module (behind --experimental-vm-modules, the same module compile node
// --check does for a .mjs), then through the CommonJS wrapper node --check
// uses for a .cjs. It prints how many it read, and a count that disagrees with
// the list fails the run. Only a file that fails both is handed to node --check
// as a .mjs copy, so its message is node's own, naming the line and the token.
//
// A MISSING node IS A FAILURE, NOT A SKIP. "Skip when the tool is absent" is
// how a check reports green on code it never looked at. CI runners ship node.
//
// Test bundles and minified files are included: they are JavaScript too, and
// cost nothing to parse.

// nodeBin is the node this rule runs. A test points it at a name that does not
// exist, to prove that a missing node fails the run.
var nodeBin = "node"

// parseScript reads NUL-separated paths on stdin and prints
// {"checked": n, "failed": [paths]}. Exit 3: this node cannot parse a module
// without running it.
var parseScript = strings.ReplaceAll(`
const vm = require('vm');
const fs = require('fs');
if (typeof vm.SourceTextModule !== 'function') {
  process.stderr.write('vm.SourceTextModule is missing in node ' + process.version);
  process.exit(3);
}
const params = ['exports', 'require', 'module', '__filename', '__dirname'];
const files = fs.readFileSync(0, 'utf8').split('\0').filter(Boolean);
const failed = [];
let checked = 0;
for (const f of files) {
  const src = fs.readFileSync(f, 'utf8');
  checked++;
  try { new vm.SourceTextModule(src, { identifier: f }); continue; } catch (e) {}
  try { vm.compileFunction(src, params, { filename: f }); continue; } catch (e) {}
  failed.push(f);
}
process.stdout.write(JSON.stringify({ checked: checked, failed: failed }));
`, "\n", " ")

// jsFiles lists the tracked .js files on disk, outside node_modules.
func jsFiles(t *tree) []string {
	var out []string
	for _, f := range t.files {
		if strings.HasSuffix(f.path, ".js") && f.data != nil && !strings.Contains("/"+f.path, "/node_modules/") {
			out = append(out, f.path)
		}
	}
	return out
}

// parseInNode runs parseScript in root over files and returns the ones that
// parse under neither grammar.
func parseInNode(root string, files []string) ([]string, error) {
	cmd := exec.Command(nodeBin, "--experimental-vm-modules", "--no-warnings", "-e", parseScript)
	cmd.Dir = root
	cmd.Stdin = strings.NewReader(strings.Join(files, "\x00"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 3 {
		return nil, fmt.Errorf("%s, so a module cannot be parsed without running it", strings.TrimSpace(stderr.String()))
	}
	if err != nil {
		return nil, fmt.Errorf("node did not finish the parse (%v): %s", err, strings.TrimSpace(stderr.String()))
	}
	var res struct {
		Checked int      `json:"checked"`
		Failed  []string `json:"failed"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, fmt.Errorf("node's answer is not the parse report: %v: %.200q", err, out)
	}
	if res.Checked != len(files) {
		return nil, fmt.Errorf("node read %d of the %d .js files, so the rest were NOT verified", res.Checked, len(files))
	}
	return res.Failed, nil
}

var checkLineRe = regexp.MustCompile(`candidate\.mjs:([0-9]+)`)

// explain is one unparseable file as a finding, with node --check's own
// message from a .mjs copy (the grammar the guard reported).
func explain(root, path string) Finding {
	f := Finding{File: path, Why: "does not parse as an ES module or as CommonJS"}
	dir, err := os.MkdirTemp("", "repolint-js")
	if err != nil {
		return f
	}
	defer os.RemoveAll(dir)
	src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	cand := filepath.Join(dir, "candidate.mjs")
	if err != nil || os.WriteFile(cand, src, 0o600) != nil {
		return f
	}
	// node prints "<file>:<line>", the source line, a caret line, a blank line,
	// then the error.
	out, _ := exec.Command(nodeBin, "--check", cand).CombinedOutput()
	lines := strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n")
	if m := checkLineRe.FindStringSubmatch(lines[0]); m != nil {
		fmt.Sscan(m[1], &f.Line)
	}
	for i := 3; i < len(lines); i++ {
		if strings.Contains(lines[i], "Error") {
			f.Why += ": " + strings.TrimSpace(lines[i])
			f.Detail = strings.Join(lines[1:3], "\n")
			return f
		}
	}
	f.Detail = strings.TrimSpace(string(out))
	return f
}

func jsParse(t *tree) result {
	files := jsFiles(t)
	ok := fmt.Sprintf("%d files", len(files))
	if _, err := exec.LookPath(nodeBin); err != nil {
		return result{[]Finding{{Why: "node is not installed, so the JS was NOT verified. This is a failure and not a skip: " +
			"a syntax error in a shipped bundle blanks every operator HMI. Install Node, then re-run."}}, ok}
	}
	failed, err := parseInNode(t.root, files)
	if err != nil {
		return result{[]Finding{{Why: err.Error()}}, ok}
	}
	var fs []Finding
	for _, p := range failed {
		fs = append(fs, explain(t.root, p))
	}
	return result{fs, ok}
}
