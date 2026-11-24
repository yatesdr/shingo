package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"strconv"
	"strings"
)

// The reads the reuse wrapper (shared/scripts/gatetime/reuse) cannot see on
// Windows. There it learns what a test binary read from Go's test log alone,
// which package os writes, and only from m.Run on: no path trace exists there
// (the Linux tracer sees both). A pass that read through anything else, or
// before m.Run, can be served again after the file changed: probes 23 and 25
// of the reuse probe set came out RUN,REUSE,REUSE on Windows where FAIL was
// wanted (work/loc-census-2026-11-20/gate-fast/lanes/reuse/raw/probes-windows-1.log).
// The design rounds left that residual to lint (R30): these rules.

// --- raw-syscall-reads -----------------------------------------------------
//
// Test code reads files through package os, never through a raw syscall: no
// direct call of an open, read or stat family function, or of the Syscall and
// RawSyscall families, of syscall, golang.org/x/sys/unix or
// golang.org/x/sys/windows (rawReadFuncs says which), and no DLL loaded there
// (a loaded proc's Call is a raw syscall the rule cannot name).
//
// WHY. A raw read is in no test log, so on Windows the wrapper keys the run on
// what package os saw and serves the pass again after the file changes: probe
// 23 (a test reading through syscall.Open). Through os the open is logged and
// its content is keyed.
//
// WHAT IS TEST CODE. Every _test.go file of the five modules, and the non-test
// files of a test-only package (testdb-shaped helpers): not main, imported by
// at least one file of another directory, and only by test files or by other
// test-only packages. Every other package is product or tool code, and its raw
// calls are not read, on purpose: the census at 10f1b27f found raw syscall
// calls only in the reuse wrapper itself (shared/scripts/gatetime and its
// reuse package: the Linux tracer, statx for a birth time, the Windows job
// object and change time), which is a main program's code and the instrument,
// not a test's. The probe set's syscall.Open is the text of a source the probe
// writes into a throwaway module: a string literal, which a rule that reads
// calls never sees as one.
var rawSyscallPkgs = map[string]bool{"syscall": true, "golang.org/x/sys/unix": true, "golang.org/x/sys/windows": true}

// rawReadFuncs are the open, read and stat families and the DLL loaders, by
// name, across the three packages. Any name starting with Syscall or
// RawSyscall is in as well (rawRead).
var rawReadFuncs = map[string]bool{
	// open
	"Open": true, "Openat": true, "Openat2": true, "Creat": true, "OpenByHandleAt": true,
	"CreateFile": true, "NtCreateFile": true, "NtOpenFile": true,
	// read
	"Read": true, "Pread": true, "Readv": true, "Preadv": true, "Preadv2": true, "ReadFile": true, "NtReadFile": true,
	"Readlink": true, "Readlinkat": true, "ReadDirent": true, "Getdents": true, "Getdirentries": true,
	"FindFirstFile": true, "FindNextFile": true, "NtQueryDirectoryFile": true, "ReadDirectoryChanges": true,
	"Mmap": true, "CreateFileMapping": true, "MapViewOfFile": true,
	// stat
	"Stat": true, "Lstat": true, "Fstat": true, "Fstatat": true, "Statx": true, "Statfs": true, "Fstatfs": true,
	"Access": true, "Faccessat": true, "Faccessat2": true, "Getxattr": true, "Lgetxattr": true, "Fgetxattr": true,
	"Listxattr": true, "Llistxattr": true, "GetFileAttributes": true, "GetFileAttributesEx": true,
	"GetFileInformationByHandle": true, "GetFileInformationByHandleEx": true,
	// a DLL's procs are called through Call, which names no function
	"LoadDLL": true, "MustLoadDLL": true, "NewLazyDLL": true, "NewLazySystemDLL": true, "LoadLibrary": true,
	"LoadLibraryEx": true,
}

func rawRead(name string) bool {
	return rawReadFuncs[name] || strings.HasPrefix(name, "Syscall") || strings.HasPrefix(name, "RawSyscall")
}

// importNames maps the names a file imports packages under to their paths;
// a blank or dot import names nothing a selector can reach.
func importNames(af *ast.File) map[string]string {
	m := map[string]string{}
	for _, im := range af.Imports {
		p, err := strconv.Unquote(im.Path.Value)
		if err != nil {
			continue
		}
		name := p[strings.LastIndexByte(p, '/')+1:]
		if im.Name != nil {
			name = im.Name.Name
		}
		if name != "_" && name != "." {
			m[name] = p
		}
	}
	return m
}

// pkgCall is the package path and function of a call written pkg.Func(...),
// where pkg is a name the file imports.
func pkgCall(c *ast.CallExpr, imps map[string]string) (pkg, fn string, ok bool) {
	sel, isSel := c.Fun.(*ast.SelectorExpr)
	if !isSel {
		return "", "", false
	}
	id, isID := sel.X.(*ast.Ident)
	if !isID || imps[id.Name] == "" {
		return "", "", false
	}
	return imps[id.Name], sel.Sel.Name, true
}

// lineOf is the 1-based line of pos in a file parsed alone (parseAll gives
// each file a FileSet of its own, so its offsets start at af.FileStart).
func lineOf(data []byte, af *ast.File, pos token.Pos) int {
	off := int(pos - af.FileStart)
	if off < 0 || off > len(data) {
		return 0
	}
	return 1 + bytes.Count(data[:off], []byte("\n"))
}

// goImports is what one file imports, read with parser.ImportsOnly.
type goImports struct {
	f       *file
	pkgName string
	paths   []string
}

func importsOf(f *file) (goImports, error) {
	af, err := parser.ParseFile(token.NewFileSet(), f.path, f.data, parser.ImportsOnly)
	if err != nil {
		return goImports{f: f}, err
	}
	gi := goImports{f: f, pkgName: af.Name.Name}
	for _, im := range af.Imports {
		if p, err := strconv.Unquote(im.Path.Value); err == nil {
			gi.paths = append(gi.paths, p)
		}
	}
	return gi, nil
}

// testOnly answers whether a package directory is test-only: not main, and
// imported only by test files or by other test-only packages.
type testOnly struct {
	files   []*file
	modules map[string]string // module directory -> module path, from each module's go.mod
	memo    map[string]bool
	bad     []Finding
}

func newTestOnly(t *tree, files []*file) *testOnly {
	to := &testOnly{files: files, modules: map[string]string{}, memo: map[string]bool{}}
	for _, f := range t.files {
		dir, base := path.Split(f.path)
		if base != "go.mod" || f.data == nil || !inModule(f.path) {
			continue
		}
		eachLine(f.data, func(_ int, line []byte) {
			if rest, ok := strings.CutPrefix(strings.TrimSpace(string(line)), "module "); ok {
				to.modules[strings.TrimSuffix(dir, "/")] = strings.Trim(strings.TrimSpace(rest), `"`)
			}
		})
	}
	return to
}

// importPath is a package directory's import path, "" outside every module.
func (to *testOnly) importPath(dir string) string {
	best, bestMod := "", ""
	for md, mp := range to.modules {
		if (dir == md || strings.HasPrefix(dir, md+"/")) && len(md) > len(best) {
			best, bestMod = md, mp
		}
	}
	if bestMod == "" {
		return ""
	}
	return bestMod + strings.TrimPrefix(dir, best)
}

func (to *testOnly) is(dir string) bool {
	if v, ok := to.memo[dir]; ok {
		return v
	}
	to.memo[dir] = false // an import cycle is no evidence of test-only
	ip := to.importPath(dir)
	if ip == "" {
		return false
	}
	quoted := []byte(strconv.Quote(ip))
	importers, isMain := 0, false
	for _, f := range to.files {
		if path.Dir(f.path) == dir && !strings.HasSuffix(f.path, "_test.go") && !isMain {
			gi, err := importsOf(f)
			isMain = err == nil && gi.pkgName == "main"
		}
		// The package's own external tests import it too; they are not users.
		if path.Dir(f.path) == dir || !bytes.Contains(f.data, quoted) {
			continue
		}
		gi, err := importsOf(f)
		if err != nil {
			to.bad = append(to.bad, Finding{File: f.path, Why: "does not parse, so what it imports is unknown: " + err.Error()})
			return false
		}
		for _, p := range gi.paths {
			if p != ip {
				continue
			}
			importers++
			if !strings.HasSuffix(f.path, "_test.go") && !to.is(path.Dir(f.path)) {
				return false
			}
		}
	}
	to.memo[dir] = importers > 0 && !isMain
	return to.memo[dir]
}

func rawSyscallReads(t *tree) result {
	files := moduleGo(t)
	to := newTestOnly(t, files)
	var scoped []*file
	var fs []Finding
	nTest, nHelper := 0, 0
	for _, f := range files {
		if !bytes.Contains(f.data, []byte(`"syscall"`)) && !bytes.Contains(f.data, []byte(`"golang.org/x/sys/`)) {
			continue
		}
		gi, err := importsOf(f)
		if err != nil {
			fs = append(fs, Finding{File: f.path, Why: "does not parse, so its calls cannot be read: " + err.Error()})
			continue
		}
		raw := false
		for _, p := range gi.paths {
			raw = raw || rawSyscallPkgs[p]
		}
		switch {
		case !raw:
		case strings.HasSuffix(f.path, "_test.go"):
			scoped = append(scoped, f)
			nTest++
		case len(to.modules) == 0:
			// No import path can be named, so no package can be found test-only:
			// say so rather than read this file as product code.
			fs = append(fs, Finding{File: f.path, Why: "no module's go.mod was read, so whether this is test-only helper code is unknown"})
		case to.is(path.Dir(f.path)):
			scoped = append(scoped, f)
			nHelper++
		}
	}
	var calls []Finding
	bad := parseAll(scoped, func(f *file, af *ast.File) {
		imps := importNames(af)
		ast.Inspect(af, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if pkg, fn, ok := pkgCall(c, imps); ok && rawSyscallPkgs[pkg] && rawRead(fn) {
				calls = append(calls, Finding{File: f.path, Line: lineOf(f.data, af, c.Pos()),
					Why: fmt.Sprintf("calls %s.%s in test code: a raw read is in no test log, so the reuse wrapper cannot see it on Windows", pkg, fn)})
			}
			return true
		})
	})
	fs = append(append(append(fs, to.bad...), bad...), calls...)
	return result{fs, fmt.Sprintf("%d test files and %d test-only helper files import a raw syscall package, %d module paths read; no read through one",
		nTest, nHelper, len(to.modules))}
}

// --- init-reads ------------------------------------------------------------
//
// Nothing touches the file system or starts a process while a package
// initialises: no direct call of package os (env reads aside), io/ioutil or
// os/exec, nor of the readers path/filepath (Glob, Walk, WalkDir,
// EvalSymlinks) and the template packages (ParseFiles, ParseGlob) offer, in
// an init func or a package-level var's initializer, in any Go file of the
// five modules.
//
// WHY. A package initialises before m.Run, and Go's test log starts at m.Run,
// so on Windows the reuse wrapper never sees such a read and serves the pass
// again after the file changes: probe 25 (a package-level var reading a file
// with os.ReadFile). Every package's initialisation runs in its own test binary
// and in every test binary that links it, so product files count as well as
// test files. A TestMain that reads before m.Run is not this rule's: the
// wrapper keys a package with a TestMain on the whole tree (probe 24).
//
// ENV READS ARE ALLOWED. os.Getenv, os.LookupEnv, os.Environ and os.ExpandEnv
// read the environment, not the file system, and the wrapper keys the whole
// environment block the binary receives (reuse.Key's Env, keyEnv), not only the
// names the test log shows: probes 19 and 20 (an env var read in a package var,
// os.Environ) are ok on Windows. A value it masks (the Postgres port, the
// template name, the private temp dir) is run scoped by design, before m.Run
// or after it alike.
//
// DIRECT CALLS ONLY (citrine's line). A call written in the init body or the
// initializer, including inside a function literal called on the spot
// (var x = func() T { ... }()), fires; a call inside a helper the initializer
// calls does not, and neither does one in a function literal stored for later
// (a handler registered in init runs when it is called). The census at
// 10f1b27f found one helper read, on purpose: the reuse canary's package var,
// which is there to show the Linux trace sees what Windows cannot.
var initReadPkgs = map[string]func(fn string) bool{
	"os":            func(fn string) bool { return !initEnvReads[fn] },
	"io/ioutil":     func(string) bool { return true },
	"os/exec":       func(string) bool { return true },
	"path/filepath": func(fn string) bool { return fn == "Glob" || fn == "Walk" || fn == "WalkDir" || fn == "EvalSymlinks" },
	"text/template": func(fn string) bool { return fn == "ParseFiles" || fn == "ParseGlob" },
	"html/template": func(fn string) bool { return fn == "ParseFiles" || fn == "ParseGlob" },
}

var initEnvReads = map[string]bool{"Getenv": true, "LookupEnv": true, "Environ": true, "ExpandEnv": true}

// initCalls returns the banned calls written directly in n: it does not enter
// a function literal unless the literal is called right there.
func initCalls(n ast.Node, imps map[string]string, each func(*ast.CallExpr, string, string)) {
	ast.Inspect(n, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.CallExpr:
			if lit, ok := x.Fun.(*ast.FuncLit); ok {
				initCalls(lit.Body, imps, each)
				for _, a := range x.Args {
					initCalls(a, imps, each)
				}
				return false
			}
			if pkg, fn, ok := pkgCall(x, imps); ok && initReadPkgs[pkg] != nil && initReadPkgs[pkg](fn) {
				each(x, pkg, fn)
			}
		}
		return true
	})
}

func initReads(t *tree) result {
	var files []*file
	for _, f := range moduleGo(t) {
		for pkg := range initReadPkgs {
			if bytes.Contains(f.data, []byte(strconv.Quote(pkg))) {
				files = append(files, f)
				break
			}
		}
	}
	var calls []Finding
	inits, vars := 0, 0
	bad := parseAll(files, func(f *file, af *ast.File) {
		imps := importNames(af)
		report := func(where string) func(*ast.CallExpr, string, string) {
			return func(c *ast.CallExpr, pkg, fn string) {
				calls = append(calls, Finding{File: f.path, Line: lineOf(f.data, af, c.Pos()),
					Why: fmt.Sprintf("%s calls %s.%s before m.Run: no test log records it, so the reuse wrapper cannot see it on Windows", where, pkg, fn)})
			}
		}
		for _, d := range af.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil && d.Name.Name == "init" && d.Body != nil {
					inits++
					initCalls(d.Body, imps, report("init"))
				}
			case *ast.GenDecl:
				if d.Tok != token.VAR {
					continue
				}
				for _, s := range d.Specs {
					vs := s.(*ast.ValueSpec)
					if len(vs.Values) == 0 {
						continue
					}
					vars++
					for _, v := range vs.Values {
						initCalls(v, imps, report("var "+vs.Names[0].Name))
					}
				}
			}
		}
	})
	fs := append(bad, calls...)
	return result{fs, fmt.Sprintf("%d files that import a reading package: %d init funcs, %d package-level var initializers, no read in one",
		len(files), inits, vars)}
}
