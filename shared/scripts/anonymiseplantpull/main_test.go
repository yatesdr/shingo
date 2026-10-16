package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The goldens were produced by the anonymiser this program replaced, from
// the same synthetic pull, and are byte-identical to its output apart from
// the note line naming the program. The pull is invented end to end and is
// built to reach every edge the byte format has: integer and float
// coordinates, nulls and absent columns, exponent floats, -0, an integer
// wider than 64 bits, a duplicate key, control and astral characters, both
// shapes of allowed_payload_codes, the sentinels, every timestamp column, and
// the vendor blob the program drops.
func TestRun_MatchesTheGolden(t *testing.T) {
	for _, tag := range []string{"A", "B"} {
		t.Run(tag, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "out.json")
			var stderr bytes.Buffer
			if rc := run([]string{"--in", "testdata/pull.json", "--out", out, "--tag", tag}, &stderr); rc != 0 {
				t.Fatalf("exit %d: %s", rc, stderr.String())
			}
			got, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile("testdata/want-" + tag + ".json")
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("output differs from testdata/want-%s.json:\n%s", tag, got)
			}
			if !strings.Contains(stderr.String(), "(6 points, 2 edges, 4 claims, 3 styles, 21 source strings checked)") {
				t.Errorf("summary line: %s", stderr.String())
			}
		})
	}
}

// A column nobody renamed fails the run instead of shipping, and nothing is
// written.
func TestRun_RefusesASurvivingSourceValue(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out.json")
	var stderr bytes.Buffer
	if rc := run([]string{"--in", "testdata/pull-leaks.json", "--out", out, "--tag", "A"}, &stderr); rc != 1 {
		t.Fatalf("exit %d, want 1: %s", rc, stderr.String())
	}
	for _, want := range []string{
		"REFUSING TO WRITE " + out + " — 1 source value(s) survived:\n",
		"   'INV-PART-ONE' at .style_node_claims[].auto_request_payload\n",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr.String())
		}
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("a refused run wrote %s", out)
	}
}

func TestRun_AnUnknownTagIsAUsageError(t *testing.T) {
	var stderr bytes.Buffer
	if rc := run([]string{"--in", "testdata/pull.json", "--out", "x", "--tag", "C"}, &stderr); rc != 2 {
		t.Fatalf("exit %d, want 2", rc)
	}
}

func TestRepr(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"plain", `'plain'`},
		{"it's", `"it's"`},
		{`a'b"c`, `'a\'b"c'`},
		{"tab\there", `'tab\there'`},
		{"\x01é", `'\x01é'`},
	} {
		if got := repr(c.in); got != c.want {
			t.Errorf("repr(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}
