package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// --- js-undeclared ---------------------------------------------------------
//
// Every page script under a www/static directory reads only names something
// declares: a var, let, const, function, class, parameter or import in scope,
// a browser global, a name the page sets on window, or a global a template's
// inline script declares.
//
// WHY. A module is strict, so reading a name nothing declares is a
// ReferenceError, and js-parse cannot see it: the file parses. The node pop-up
// read openNodeModal's opts inside loadNodeDetail, which was never passed it
// (2026-10-08). The error stopped the pop-up before any stored setting reached
// the form, and every Save on a group wrote the defaults back, at both plants,
// for a month, with nothing on screen.
//
// TESTS AND VENDORED FILES ARE LEFT OUT. A *.test.js runs under node with the
// names its harness injects; a vendored or minified file is not ours to keep.
//
// THE WALK IS jsundeclared.js, one node process for every file, on node's own
// bundled parser. The known browser globals are a fixed list there.

//go:embed jsundeclared.js
var undeclaredScript string

// pageScripts lists the tracked page scripts the rule reads.
func pageScripts(t *tree) []*file {
	var out []*file
	for _, f := range t.files {
		p := "/" + f.path
		if f.data == nil || !strings.HasSuffix(p, ".js") || !strings.Contains(p, "/www/static/") {
			continue
		}
		if strings.HasSuffix(p, ".test.js") || strings.HasSuffix(p, ".min.js") || strings.Contains(p, "/vendor/") {
			continue
		}
		out = append(out, f)
	}
	return out
}

// templates are the tracked www templates, read for their inline scripts.
func templates(t *tree) []string {
	var out []string
	for _, f := range t.files {
		if f.data != nil && strings.HasSuffix(f.path, ".html") && strings.Contains("/"+f.path, "/www/templates/") {
			out = append(out, string(f.data))
		}
	}
	return out
}

type undeclaredReport struct {
	Checked  int `json:"checked"`
	Skipped  int `json:"skipped"`
	Findings []struct {
		Path string `json:"path"`
		Line int    `json:"line"`
		Name string `json:"name"`
	} `json:"findings"`
}

// walkInNode runs the walk over files and returns node's report.
func walkInNode(files []*file, tmpl []string) (undeclaredReport, error) {
	var rep undeclaredReport
	type src struct {
		Path string `json:"path"`
		Src  string `json:"src"`
	}
	in := struct {
		Files     []src    `json:"files"`
		Templates []string `json:"templates"`
	}{Templates: tmpl}
	for _, f := range files {
		in.Files = append(in.Files, src{f.path, string(f.data)})
	}
	body, err := json.Marshal(in)
	if err != nil {
		return rep, err
	}
	script, err := os.CreateTemp("", "repolint-undeclared-*.js")
	if err != nil {
		return rep, err
	}
	defer os.Remove(script.Name())
	if _, err := script.WriteString(undeclaredScript); err != nil {
		script.Close()
		return rep, err
	}
	script.Close()

	cmd := exec.Command(nodeBin, "--expose-internals", "--no-warnings", script.Name())
	cmd.Stdin = bytes.NewReader(body)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 3 {
		return rep, fmt.Errorf("%s, so the JS was NOT checked", strings.TrimSpace(stderr.String()))
	}
	if err != nil {
		return rep, fmt.Errorf("node did not finish the walk (%v): %s", err, strings.TrimSpace(stderr.String()))
	}
	if err := json.Unmarshal(out, &rep); err != nil {
		return rep, fmt.Errorf("node's answer is not the walk's report: %v: %.200q", err, out)
	}
	if rep.Checked+rep.Skipped != len(files) {
		return rep, fmt.Errorf("node read %d of the %d page scripts, so the rest were NOT checked", rep.Checked+rep.Skipped, len(files))
	}
	return rep, nil
}

func jsUndeclared(t *tree) result {
	files := pageScripts(t)
	ok := fmt.Sprintf("%d page scripts", len(files))
	if _, err := exec.LookPath(nodeBin); err != nil {
		return result{[]Finding{{Why: "node is not installed, so the JS was NOT checked. This is a failure and not a skip. Install Node, then re-run."}}, ok}
	}
	rep, err := walkInNode(files, templates(t))
	if err != nil {
		return result{[]Finding{{Why: err.Error()}}, ok}
	}
	if rep.Skipped > 0 {
		ok += fmt.Sprintf("; %d that do not parse are js-parse's", rep.Skipped)
	}
	var fs []Finding
	for _, f := range rep.Findings {
		fs = append(fs, Finding{File: f.Path, Line: f.Line,
			Why: "reads " + f.Name + ", which nothing declares: a ReferenceError when the line runs"})
	}
	return result{fs, ok}
}
