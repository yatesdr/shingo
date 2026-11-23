package main

// `gatetime exec` is go test -exec's program, RECORD ONLY: it runs one test
// binary and writes down what that run read. It never decides a verdict, never
// reuses one, and never changes what the binary prints or how it exits:
// stdin, stdout and stderr are the wrapper's own handles, inherited, and the
// exit code is the binary's.
//
// What it adds to the binary's run, and nothing else:
//   - a job object of its own (job_windows.go): exact CPU of the binary and
//     everything it started, and how many processes that was;
//   - when $GATETIME_MANIFESTS names a directory, -test.testlogfile=<temp
//     file> at the front of the arguments. go test asks for no test log under
//     -exec (cmd/go/internal/test, testlogArg), so without this the testing
//     package records nothing. The log is the testing package's own record of
//     getenv, open, stat and chdir made through package os during m.Run.
//
// After the binary exits it writes one manifest (manifest.go) into
// $GATETIME_MANIFESTS: the binary's sha256, the package, argv, cwd, the env
// vars read (values hashed), every path read with its content hash, in or out
// of the module, the child count, wall, CPU, the exit code and the identity
// hash of the gate run ($GATETIME_IDENTITY, a file the gate writes once).
//
// KNOWN HOLES, which no manifest can show and every reader must assume:
//   - reads before m.Run: package init, package-level vars, and a TestMain's
//     work before it calls m.Run (the test log starts inside m.Run);
//   - a child's own reads: node, git, bash or go started by a test read what
//     they like. Children > 0 is how this hole is seen: a binary that started
//     a child has an unobserved read set;
//   - reads not made through package os: raw syscalls, cgo, a memory map made
//     without os.Open;
//   - env read through os.Environ, which the testing package does not log
//     (a test that hands os.Environ() to a child reads every variable);
//   - content is hashed after the run, not at the moment of the read: a file
//     the run changed is marked changed_in_run and its hash is the run's
//     output.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"time"

	"shingo/shared/scripts/gatetime/reuse"
)

// execMain runs go test's binary: args[0] is the binary, the rest its
// arguments.
func execMain(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: gatetime exec BINARY ARGS...   (go test -exec's program)")
		return 2
	}
	// Reuse (phase 3) is a separate path, taken only when switched on: with
	// $GATETIME_REUSE unset, nothing below this line changes.
	if os.Getenv("GATETIME_REUSE") != "" {
		return reuse.Main(args, reuseRunner)
	}
	// The binary decides what an interrupt means, as it did with no wrapper:
	// the console delivers it to both, and the wrapper waits for the binary.
	signal.Ignore(os.Interrupt)

	dir := os.Getenv("GATETIME_MANIFESTS")
	m := &Manifest{Schema: manifestSchema, Binary: args[0], Argv: append([]string{}, args[1:]...)}
	argv := args[1:]
	var logPath string
	if dir != "" {
		f, err := os.CreateTemp("", "gatetime-testlog-*.txt")
		if err == nil {
			logPath = f.Name()
			err = f.Close()
		}
		if err != nil {
			m.Errors = append(m.Errors, "test log: "+err.Error())
		} else {
			m.TestlogArg = "-test.testlogfile=" + logPath
			argv = append([]string{m.TestlogArg}, argv...)
		}
	}

	cmd := exec.Command(args[0], argv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	t0 := time.Now()
	run, err := startInJob(cmd)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gatetime exec: could not start the test binary:", err)
		removeTemp(logPath)
		return 1
	}
	waitErr, cpu, children, jobNote := run.wait()
	wall := time.Since(t0)
	code := exitFor(waitErr)

	if dir != "" {
		m.Start = t0.UTC().Format(time.RFC3339Nano)
		m.WallS, m.Exit, m.Children, m.Job = wall.Seconds(), code, children, jobNote
		m.CPUS = -1
		if cpu >= 0 {
			m.CPUS = cpu.Seconds()
		}
		record(dir, m, logPath, t0)
	}
	removeTemp(logPath)
	return code
}

// record fills in what is read after the run and writes the manifest. A
// manifest that cannot be written is said on stderr: it is the one channel
// left, and a missing manifest must not read as a binary that read nothing.
func record(dir string, m *Manifest, logPath string, t0 time.Time) {
	m.Cwd, _ = os.Getwd()
	m.ModuleRoot, m.Package = modulePackage(m.Cwd)
	m.Cwd, m.ModuleRoot = filepath.ToSlash(m.Cwd), filepath.ToSlash(m.ModuleRoot)
	if s, err := sha256File(m.Binary); err != nil {
		m.Errors = append(m.Errors, "binary hash: "+err.Error())
	} else {
		m.BinarySHA256 = s
	}
	if self, err := os.Executable(); err == nil {
		if s, err := sha256File(self); err == nil {
			m.ToolSHA256 = s
		}
	}
	m.IdentityFile = os.Getenv("GATETIME_IDENTITY")
	m.IdentitySHA256 = "none"
	if m.IdentityFile != "" {
		if data, err := os.ReadFile(m.IdentityFile); err != nil {
			m.IdentitySHA256 = "error: " + err.Error()
		} else {
			sum := sha256.Sum256(data)
			m.IdentitySHA256 = hex.EncodeToString(sum[:])
		}
	}
	var data []byte
	if logPath != "" {
		if b, err := os.ReadFile(logPath); err == nil {
			data = b
		}
	}
	// The binary's own directory as the first pwd, before any chdir. The
	// manifest's Cwd is in forward slashes; the parse wants the OS's form.
	m.fillReads(parseTestlog(data, filepath.FromSlash(m.Cwd)), t0)
	if _, err := writeManifest(dir, m); err != nil {
		fmt.Fprintln(os.Stderr, "gatetime exec: manifest not written:", err)
	}
}

// removeTemp removes the wrapper's test log; "" is no file.
func removeTemp(p string) {
	if p != "" {
		_ = os.Remove(p)
	}
}
