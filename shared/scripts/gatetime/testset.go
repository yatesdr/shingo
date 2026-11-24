package main

// `gatetime testset -tags TAGS -record FILE` sits between `go test -json` and
// the log a gate step writes, when the gate runs with GATE_TESTSET=on
// (scripts/gate-testsets.sh): it prints the text the step's log has always
// held, and appends one line per verdict to FILE,
//
//	<tags> TAB <package> TAB <test> TAB <pass|fail|skip>
//
// with "-" as the test for the package's own verdict. The text is every output
// and build-output event's Output, in order; a line that is not a JSON event
// (go's own stderr, which the gate merges in) passes through unchanged. Under
// -json go test asks the binary for its verbose stream, so the log also holds
// the `=== RUN` and `--- PASS` lines a plain run leaves out; the gate's readers
// (excerpt_failures, the steps log's `ok pkg Ns` lines) read it the same.
//
// The records' reader is testsets_compare (scripts/gate-testsets.sh), which
// holds the Windows side's set against the Linux side's for one tree.

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
)

func testsetMain(args []string) int {
	fs := flag.NewFlagSet("testset", flag.ContinueOnError)
	tags := fs.String("tags", "none", "the -tags the go test ran with, for the record")
	record := fs.String("record", "", "the file each verdict is appended to")
	if err := fs.Parse(args); err != nil || *record == "" || fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "usage: go test -json ... | gatetime testset -tags TAGS -record FILE")
		return 2
	}
	f, err := os.OpenFile(*record, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gatetime testset:", err)
		return 1
	}
	err = testset(os.Stdin, os.Stdout, f, *tags)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gatetime testset:", err)
		return 1
	}
	return 0
}

// testEvent is the part of a test2json event testset reads.
type testEvent struct {
	Action  string `json:"Action"`
	Package string `json:"Package"`
	Test    string `json:"Test"`
	Output  string `json:"Output"`
}

// testset copies in to out as text and appends each verdict to rec.
func testset(in io.Reader, out, rec io.Writer, tags string) error {
	r := bufio.NewReaderSize(in, 1<<20)
	w := bufio.NewWriter(out)
	rw := bufio.NewWriter(rec)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			if werr := testsetLine(line, w, rw, tags); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return rw.Flush()
}

func testsetLine(line []byte, w, rw *bufio.Writer, tags string) error {
	var e testEvent
	if line[0] != '{' || json.Unmarshal(line, &e) != nil || e.Action == "" {
		_, err := w.Write(line)
		return err
	}
	switch e.Action {
	case "output", "build-output":
		_, err := w.WriteString(e.Output)
		return err
	case "pass", "fail", "skip":
		if e.Package == "" {
			return nil
		}
		test := e.Test
		if test == "" {
			test = "-"
		}
		_, err := fmt.Fprintf(rw, "%s\t%s\t%s\t%s\n", tags, e.Package, test, e.Action)
		return err
	}
	return nil
}
