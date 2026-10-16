package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCount(t *testing.T) {
	root := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("modA/pkg/a_test.go", "package pkg\n\nfunc T() {\n\tv, _ := f()\n\t_ = g()\n\ta, _, _ = h()\n\tx, _ = y()\n\tdefer c.Close()\n\tq,_:=z()\n}\n")
	write("modB/b_test.go", "package pkg\r\nfunc U() {\r\n\tv, _ := f()\r\n}\r\n")
	write("modA/pkg/c.go", "package pkg\nfunc V() {\n\tv, _ := f()\n}\n")
	write("findings.txt", `modA/pkg/a_test.go:4:2: Error return value is not checked
modA/pkg/a_test.go:4:5: the same site at another column
modA/pkg/a_test.go:5:2: a blank alone
modA/pkg/a_test.go:6:2: three values
modA/pkg/a_test.go:7:2: a plain assignment
modA/pkg/a_test.go:8:2: a defer
modA/pkg/a_test.go:9:2: no spaces
modA/pkg/a_test.go:99:2: past the end
  modB/b_test.go:3:2: leading space, CRLF file
modA/pkg/c.go:3:2: not a test file
modA/pkg/missing_test.go:1:1: no such file
not a finding line
`)
	n, err := count(root, filepath.Join(root, "findings.txt"))
	if err != nil {
		t.Fatal(err)
	}
	// a_test.go lines 4 (once), 6, 7 and 9, and b_test.go line 3.
	if n != 5 {
		t.Errorf("count = %d, want 5", n)
	}
}
