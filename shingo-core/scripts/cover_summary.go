package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: go run scripts/cover_summary.go <coverage.out>")
		os.Exit(1)
	}

	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	pkgStats := map[string][2]int{}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "mode:") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 3 {
			continue
		}
		total, err1 := atoi(parts[len(parts)-2])
		covered, err2 := atoi(parts[len(parts)-1])
		if err1 != nil || err2 != nil {
			continue
		}

		location := parts[0]
		colon := strings.Index(location, ":")
		if colon >= 0 {
			location = location[:colon]
		}
		seg := strings.Split(location, "/")
		pkg := "(root)"
		if len(seg) > 1 {
			pkg = strings.Join(seg[:len(seg)-1], "/")
		}

		s := pkgStats[pkg]
		s[0] += total
		s[1] += covered
		pkgStats[pkg] = s
	}

	pkgs := make([]string, 0, len(pkgStats))
	for p := range pkgStats {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)

	fmt.Printf("%-55s %8s\n", "Package", "Coverage")
	fmt.Println(strings.Repeat("-", 64))

	var grandTotal, grandCovered int
	for _, p := range pkgs {
		s := pkgStats[p]
		grandTotal += s[0]
		grandCovered += s[1]
		pct := 0.0
		if s[0] > 0 {
			pct = float64(s[1]) / float64(s[0]) * 100
		}
		fmt.Printf("%-55s %7.1f%%\n", p, pct)
	}
	fmt.Println(strings.Repeat("-", 64))
	if grandTotal > 0 {
		fmt.Printf("%-55s %7.1f%%\n", "total", float64(grandCovered)/float64(grandTotal)*100)
	}
}

func atoi(s string) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}
