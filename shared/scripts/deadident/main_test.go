package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestQualifies(t *testing.T) {
	cases := map[string]bool{
		"planTransport":         true,
		"ApplyArrival":          true,
		"planMove":              true,  // exactly 8
		"planMov":               false, // 7
		"database":              false, // one case
		"HTTPSERVER":            false,
		"bin_type_id":           false, // SQL column
		"Prod_Counter_01":       false, // PLC tag
		"TestFoo_Bar":           true,  // a test name may carry an underscore
		"TestPlanReshuffle_":    false, // a glob prefix, `TestPlanReshuffle_*`
		"BenchmarkScan_Repo":    true,
		"MES_Line_Counter_Tags": false,
	}
	for name, want := range cases {
		if got := qualifies(name); got != want {
			t.Errorf("qualifies(%q) = %v, want %v", name, got, want)
		}
	}
}

func names(cs []citation) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.name)
	}
	return out
}

func TestCitations_Shapes(t *testing.T) {
	cases := []struct {
		text string
		want []string
	}{
		{"calls planTransport(ctx) first", []string{"planTransport"}},
		{"see BinService.ApplyArrival for the move", []string{"BinService", "ApplyArrival"}},
		{"the `applyLoaderEmptyIn` handler", []string{"applyLoaderEmptyIn"}},
		{"`ll := NewLaneLock(); ll.TryLock(lane, order)`", []string{"NewLaneLock"}},
		// A quoted value with spaces is data, not code.
		{"every row reads `Supermarket Area`, a GROUP", nil},
		// Bare prose is read only for a retired name.
		{"planRetrieveEmpty's source-group branch", []string{"planRetrieveEmpty"}},
		{"and ApplyBinArrival stages it", []string{"ApplyBinArrival"}},
		{"and applyManualSwap stages it", nil},
		{"the same name twice: planTransport( and planTransport(", []string{"planTransport"}},
		// A trailing * is a glob over a prefix, not a citation of the prefix.
		{"explicit (protocol.LoaderPositionKind*), synced", nil},
	}
	for _, c := range cases {
		if got := names(citations(c.text)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("citations(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

func TestCitations_CarryTheQualifier(t *testing.T) {
	got := citations("http.ServeContent reads it")
	want := []citation{{"http", "ServeContent"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestIsGravestone(t *testing.T) {
	for _, s := range []string{
		"planRetrieveEmpty was removed in the plan/apply cutover",
		"handleManualSwapCompletion (deleted) did this",
		"the retired loopTickFallback() path",
		"this used to call fireThresholdL1()",
		"renamed from OrderCompleted",
		"Pre-side-cycle, this called e.tryAutoRequest",
	} {
		if !isGravestone(s) {
			t.Errorf("isGravestone(%q) = false, want true", s)
		}
	}
	for _, s := range []string{
		"planTransport(ctx) plans the move",
		"the removal order clears the bin", // "removal" is not "removed"
	} {
		if isGravestone(s) {
			t.Errorf("isGravestone(%q) = true, want false", s)
		}
	}
}

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const fixtureGo = `package demo

import "net/http"

// planTransport(ctx) is live; staleHelperName(ctx) is not.
// BinService.ApplyArrival is live; BinService.ApplyStaleArrival is not.
// staleHelperName(ctx) was removed; this line is a gravestone.
// http.ServeContent belongs to another module and is not read.
// mime.ParseMediaType is stdlib the repo does not import, and is not read.
// renderBoardView() is a JS function, and live.
// staleJsHelper() is cited only in a JS comment, and dead.
// "apiEventName" is a string literal and "jsonFieldKey" a tag: both live.
// planRetrieveEmpty's bare mention is a hit: the name is on the list,
// and the literal below that makes it look live does not save it.
type BinService struct {
	Field int ` + "`json:\"jsonFieldKey\"`" + `
}

func (BinService) ApplyArrival() {}

func planTransport() string { _ = http.StatusOK; return "apiEventName" }

var _ = "planRetrieveEmpty"
`

const fixtureJS = `// staleJsHelper() used to live here
function renderBoardView() {}
`

func fixtureTree(t *testing.T) string {
	return writeTree(t, map[string]string{
		"mod/demo.go":                  fixtureGo,
		"mod/www/board.js":             fixtureJS,
		"mod/www/vendor.min.js":        "function staleJsHelper(){}",
		"mod/testdata/broken.go":       "package ( not go",
		"mod/node_modules/x/y.go":      "package ( not go",
		".git/hooks/ignored.go":        "package ( not go",
		"mod/vendor/dep/dep_vendor.go": "package dep\n// deadVendorName() is not ours\n",
	})
}

func TestScan_Fixture(t *testing.T) {
	got, err := Scan(fixtureTree(t))
	if err != nil {
		t.Fatal(err)
	}
	want := []Hit{
		{"mod/demo.go", 5, "staleHelperName"},
		{"mod/demo.go", 6, "ApplyStaleArrival"},
		{"mod/demo.go", 11, "staleJsHelper"},
		{"mod/demo.go", 13, "planRetrieveEmpty"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Scan =\n  %v\nwant\n  %v", got, want)
	}
}

func TestScan_ParseErrorFails(t *testing.T) {
	root := writeTree(t, map[string]string{"bad.go": "package ( not go"})
	if _, err := Scan(root); err == nil {
		t.Fatal("Scan on an unparseable file returned no error; it must not pass silently")
	}
}
