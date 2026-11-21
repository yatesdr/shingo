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

// --- js-imports --------------------------------------------------------------
//
// Every name a served module imports, or re-exports from another module, is
// one that module exports, and every module it names is a file in the tree.
//
// WHY. A browser links a page's whole module graph before any of it runs, and
// one import of a name its module does not export is a SyntaxError at link
// time: no module of that graph runs, and the page is blank. js-parse cannot
// see it (both files parse), js-undeclared cannot (an imported name is
// declared), and the node harnesses cannot, because they strip the imports and
// lend the names into one scope. Deleting the station's showToast with one
// unused import of it left (operator-modal.js) would have blanked every
// station, with every gate green (2026-11-21, the toasts' move onto
// shared/notice.js).
//
// WHAT IT READS: every tracked .js under a www/static directory and directly
// under shared/ (served at /static/shared/), as importers and as modules to
// import from; a vendored or minified file only as the latter. Tests are left
// out: a *.test.js is never served. jsimports.js says how a specifier
// resolves, and why import() and a classic script are left alone.

//go:embed jsimports.js
var importsScript string

// servedScripts lists the tracked files the rule reads, and whether each is
// read for its imports.
func servedScripts(t *tree) (files []*file, importer []bool) {
	for _, f := range t.files {
		p := "/" + f.path
		if f.data == nil || !strings.HasSuffix(p, ".js") || strings.HasSuffix(p, ".test.js") {
			continue
		}
		top := strings.HasPrefix(f.path, "shared/") && !strings.Contains(strings.TrimPrefix(f.path, "shared/"), "/")
		if !top && !strings.Contains(p, "/www/static/") {
			continue
		}
		files = append(files, f)
		importer = append(importer, !strings.HasSuffix(p, ".min.js") && !strings.Contains(p, "/vendor/"))
	}
	return files, importer
}

type importsReport struct {
	Checked  int `json:"checked"`
	Classic  int `json:"classic"`
	Skipped  int `json:"skipped"`
	Findings []struct {
		Path string `json:"path"`
		Line int    `json:"line"`
		Why  string `json:"why"`
	} `json:"findings"`
}

// linkInNode runs the link check over files and returns node's report.
func linkInNode(files []*file, importer []bool) (importsReport, error) {
	var rep importsReport
	type src struct {
		Path     string `json:"path"`
		Src      string `json:"src"`
		Importer bool   `json:"importer"`
	}
	in := struct {
		Files []src `json:"files"`
	}{}
	want := 0
	for i, f := range files {
		in.Files = append(in.Files, src{f.path, string(f.data), importer[i]})
		if importer[i] {
			want++
		}
	}
	body, err := json.Marshal(in)
	if err != nil {
		return rep, err
	}
	script, err := os.CreateTemp("", "repolint-imports-*.js")
	if err != nil {
		return rep, err
	}
	defer os.Remove(script.Name())
	if _, err := script.WriteString(importsScript); err != nil {
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
		return rep, fmt.Errorf("%s, so the imports were NOT checked", strings.TrimSpace(stderr.String()))
	}
	if err != nil {
		return rep, fmt.Errorf("node did not finish the link check (%v): %s", err, strings.TrimSpace(stderr.String()))
	}
	if err := json.Unmarshal(out, &rep); err != nil {
		return rep, fmt.Errorf("node's answer is not the link check's report: %v: %.200q", err, out)
	}
	if rep.Checked+rep.Classic != want {
		return rep, fmt.Errorf("node read %d of the %d modules sent, so the rest were NOT checked", rep.Checked+rep.Classic, want)
	}
	return rep, nil
}

func jsImports(t *tree) result {
	files, importer := servedScripts(t)
	if _, err := exec.LookPath(nodeBin); err != nil {
		return result{[]Finding{{Why: "node is not installed, so the imports were NOT checked. This is a failure and not a skip. Install Node, then re-run."}}, ""}
	}
	rep, err := linkInNode(files, importer)
	if err != nil {
		return result{[]Finding{{Why: err.Error()}}, ""}
	}
	ok := fmt.Sprintf("%d modules", rep.Checked)
	if rep.Skipped > 0 {
		ok += fmt.Sprintf("; %d imports from a classic script are js-parse's", rep.Skipped)
	}
	var fs []Finding
	for _, f := range rep.Findings {
		fs = append(fs, Finding{File: f.Path, Line: f.Line,
			Why: f.Why + ": the page's module graph does not link, and nothing on the page runs"})
	}
	return result{fs, ok}
}
