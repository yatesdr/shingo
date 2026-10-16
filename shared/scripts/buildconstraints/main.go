// Command buildconstraints reports a Go file whose NAME excludes it from the
// build: a trailing _<GOOS> or _<GOARCH> that no //go:build line asks for.
// scripts/check-go-file-build-constraints.sh says why it matters.
//
// Every .go file under -root is read, skipping .git, node_modules, .gate,
// vendor and testdata. A file whose last name segment is a GOOS or GOARCH
// passes only when its first 2048 characters hold a `//go:build` line naming
// that segment as a word: a DELIBERATE platform file is fine, only an accident
// is reported.
//
// Usage: go run ./scripts/buildconstraints [-root DIR]   (exit 1 on a hit)
package main

import (
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// suffixes is every GOOS and GOARCH Go recognises as a filename suffix.
var suffixes = map[string]bool{}

func init() {
	for _, s := range strings.Fields(`
aix android darwin dragonfly freebsd hurd illumos ios js linux nacl netbsd
openbsd plan9 solaris wasip1 windows zos
386 amd64 amd64p32 arm arm64 arm64be armbe loong64 mips mips64 mips64le
mips64p32 mips64p32le mipsle ppc ppc64 ppc64le riscv riscv64 s390 s390x
sparc sparc64 wasm`) {
		suffixes[s] = true
	}
}

var skipDirs = map[string]bool{".git": true, "node_modules": true, ".gate": true, "vendor": true, "testdata": true}

// constraintSuffix is the GOOS/GOARCH a file's name ends in, or "".
// Go also honours _<GOOS>_<GOARCH>; checking the last segment catches both,
// because the GOARCH half is what trails.
func constraintSuffix(name string) (suffix, stem string, test bool) {
	test = strings.HasSuffix(name, "_test.go")
	stem = strings.TrimSuffix(name, ".go")
	if test {
		stem = strings.TrimSuffix(name, "_test.go")
	}
	i := strings.LastIndexByte(stem, '_')
	if i < 0 || !suffixes[stem[i+1:]] {
		return "", stem, test
	}
	return stem[i+1:], stem, test
}

// declared reports whether the file's head carries a //go:build line naming
// suffix.
func declared(src []byte, suffix string) bool {
	text := strings.ReplaceAll(strings.ReplaceAll(string(src), "\r\n", "\n"), "\r", "\n")
	if r := []rune(text); len(r) > 2048 {
		text = string(r[:2048])
	}
	re := regexp.MustCompile(`(?m)^//go:build .*\b` + regexp.QuoteMeta(suffix) + `\b`)
	return re.MatchString(text)
}

func check(root string, w io.Writer) bool {
	fail := false
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != root && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		last, stem, test := constraintSuffix(d.Name())
		if last == "" {
			return nil
		}
		src, err := os.ReadFile(path)
		if err == nil && declared(src, last) {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		fmt.Fprintf(w, "FAIL %s\n", filepath.ToSlash(rel))
		fmt.Fprintf(w, "     ends in _%s, which Go reads as a GOOS/GOARCH build constraint, so this file is\n", last)
		if test {
			fmt.Fprintln(w, "     NOT COMPILED on other platforms and its tests silently do not exist there.")
			fmt.Fprintf(w, "     Rename it (%s_test.go -> %s_gate_test.go), or add a //go:build line naming\n", stem, stem)
		} else {
			fmt.Fprintln(w, "     NOT COMPILED on other platforms: what it declares is missing there.")
			fmt.Fprintf(w, "     Rename it (%s.go -> %s_choice.go, say), or add a //go:build line naming\n", stem, stem)
		}
		fmt.Fprintf(w, "     %s if the constraint is intended.\n", last)
		fail = true
		return nil
	})
	if !fail {
		fmt.Fprintln(w, "ok   Go filenames (no accidental GOOS/GOARCH build constraints)")
	}
	return !fail
}

func main() {
	root := flag.String("root", ".", "the repo root to walk")
	flag.Parse()
	if !check(*root, os.Stdout) {
		os.Exit(1)
	}
}
