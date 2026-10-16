// Command discardcount counts the silent error discards in test files that
// scripts/check-test-error-discard-ratchet.sh ratchets, and prints the count.
//
// Its input is that script's golangci-lint errcheck (check-blank) findings,
// one per line as `<module>/<module-relative path>:<line>:<col>: <message>`.
// A finding counts when its file is a *_test.go file and its source line
// assigns to `_` in a multi-value assignment — `v, _ :=`, `v, _ =`,
// `a, _, _ =` — the shape that reads the value afterwards. A file is linted
// once per package it belongs to, so sites are deduplicated by (file, line).
//
// Usage: go run ./scripts/discardcount -root REPO FINDINGS
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var (
	loc = regexp.MustCompile(`^(.+\.go):(\d+):\d+: `)
	// Whitespace is any space a source line can hold, Unicode ones included.
	shape = regexp.MustCompile(`,[\s\v\x1c-\x1f\x85\p{Z}]*_[\s\v\x1c-\x1f\x85\p{Z}]*:?=`)
)

type site struct {
	rel  string
	line int
}

func count(root, findings string) (int, error) {
	f, err := os.Open(findings)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	sites := map[site]bool{}
	cache := map[string][]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		m := loc.FindStringSubmatch(strings.TrimSpace(sc.Text()))
		if m == nil {
			continue
		}
		rel := strings.ReplaceAll(m[1], `\`, "/")
		if !strings.HasSuffix(rel, "_test.go") {
			continue
		}
		line, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		path := filepath.Join(root, rel)
		src, ok := cache[path]
		if !ok {
			if b, err := os.ReadFile(path); err == nil {
				text := strings.ReplaceAll(strings.ReplaceAll(string(b), "\r\n", "\n"), "\r", "\n")
				src = strings.Split(text, "\n")
			}
			cache[path] = src
		}
		if line < 1 || line > len(src) {
			continue
		}
		if shape.MatchString(src[line-1]) {
			sites[site{rel, line}] = true
		}
	}
	return len(sites), sc.Err()
}

func main() {
	root := flag.String("root", ".", "the repo root the findings' paths are relative to")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: discardcount -root REPO FINDINGS")
		os.Exit(2)
	}
	n, err := count(*root, flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(n)
}
