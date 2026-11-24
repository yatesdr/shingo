package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestTestset_TextAndRecord: the text is every output event's Output in order
// with non-JSON lines passed through, and each pass, fail and skip becomes one
// record line, the package's own as "-".
func TestTestset_TextAndRecord(t *testing.T) {
	in := strings.Join([]string{
		`{"Action":"start","Package":"shingo/x"}`,
		`{"Action":"run","Package":"shingo/x","Test":"TestA"}`,
		`{"Action":"output","Package":"shingo/x","Test":"TestA","Output":"=== RUN   TestA\n"}`,
		`{"Action":"output","Package":"shingo/x","Test":"TestA","Output":"--- PASS: TestA (0.00s)\n"}`,
		`{"Action":"pass","Package":"shingo/x","Test":"TestA","Elapsed":0}`,
		`{"Action":"output","Package":"shingo/x","Test":"TestB/sub","Output":"    b_test.go:9: skipped\n"}`,
		`{"Action":"skip","Package":"shingo/x","Test":"TestB/sub","Elapsed":0}`,
		`{"Action":"output","Package":"shingo/x","Test":"TestC","Output":"    c_test.go:3: want 1\n"}`,
		`{"Action":"fail","Package":"shingo/x","Test":"TestC","Elapsed":0}`,
		`{"Action":"output","Package":"shingo/x","Output":"FAIL\tshingo/x\t0.01s\n"}`,
		`{"Action":"fail","Package":"shingo/x","Elapsed":0.01}`,
		`{"ImportPath":"shingo/y","Action":"build-output","Output":"y.go:1:1: expected package\n"}`,
		`{"ImportPath":"shingo/y","Action":"build-fail"}`,
		`go: a line from go's own stderr`,
		`not json at all {`,
	}, "\n") + "\n"
	var out, rec bytes.Buffer
	if err := testset(strings.NewReader(in), &out, &rec, "docker,lockcheck"); err != nil {
		t.Fatal(err)
	}
	wantOut := "=== RUN   TestA\n--- PASS: TestA (0.00s)\n    b_test.go:9: skipped\n    c_test.go:3: want 1\n" +
		"FAIL\tshingo/x\t0.01s\ny.go:1:1: expected package\ngo: a line from go's own stderr\nnot json at all {\n"
	if out.String() != wantOut {
		t.Errorf("text:\n%s\nwant:\n%s", out.String(), wantOut)
	}
	wantRec := "docker,lockcheck\tshingo/x\tTestA\tpass\n" +
		"docker,lockcheck\tshingo/x\tTestB/sub\tskip\n" +
		"docker,lockcheck\tshingo/x\tTestC\tfail\n" +
		"docker,lockcheck\tshingo/x\t-\tfail\n"
	if rec.String() != wantRec {
		t.Errorf("record:\n%s\nwant:\n%s", rec.String(), wantRec)
	}
}

// TestTestset_LastLineWithoutNewline: a stream cut short still prints its tail.
func TestTestset_LastLineWithoutNewline(t *testing.T) {
	var out, rec bytes.Buffer
	if err := testset(strings.NewReader("partial line"), &out, &rec, "lockcheck"); err != nil {
		t.Fatal(err)
	}
	if out.String() != "partial line" || rec.Len() != 0 {
		t.Errorf("text %q, record %q", out.String(), rec.String())
	}
}
