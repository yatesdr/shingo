package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestMigrate_ACommandLineItDoesNotReadIsExit2: the installer tells a usage
// mistake from a refusal (exit 1) by the code, so a command line this does not
// read exits 2 with the usage on stderr, before any config or database.
func TestMigrate_ACommandLineItDoesNotReadIsExit2(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		nil,
		{"version"},
		{"check", "extra"},
		{"down"},
		{"down", "abc"},
		{"down", "0"},
		{"-nosuch", "check"},
		{"-help"},
	} {
		var stdout, stderr bytes.Buffer
		if code := runMigrate(args, &stdout, &stderr); code != 2 {
			t.Errorf("migrate %q: exit %d, want 2", args, code)
		}
		if !strings.Contains(stderr.String(), "Usage: shingocore migrate") || stdout.Len() != 0 {
			t.Errorf("migrate %q: stdout %q stderr %q, want the usage on stderr alone", args, stdout.String(), stderr.String())
		}
	}
}

// TestMigrate_ABadFlagIsNamed: a flag migrate does not read is named on
// stderr above the usage, so the person at the terminal sees which word it
// was; -help prints the usage alone.
func TestMigrate_ABadFlagIsNamed(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := runMigrate([]string{"-nosuch", "check"}, &stdout, &stderr); code != 2 ||
		!strings.HasPrefix(stderr.String(), "migrate: flag provided but not defined: -nosuch\n") {
		t.Errorf("migrate -nosuch check: exit %d stderr %q, want exit 2 and the flag named first", code, stderr.String())
	}
	stderr.Reset()
	if code := runMigrate([]string{"-help"}, &stdout, &stderr); code != 2 || !strings.HasPrefix(stderr.String(), "Usage: shingocore migrate") {
		t.Errorf("migrate -help: exit %d stderr %q, want exit 2 and the usage alone", code, stderr.String())
	}
}

// panicOnFirstWrite panics on its first Write and keeps what is written after.
type panicOnFirstWrite struct {
	bytes.Buffer
	writes int
}

func (w *panicOnFirstWrite) Write(p []byte) (int, error) {
	if w.writes++; w.writes == 1 {
		panic("the writer broke")
	}
	return w.Buffer.Write(p)
}

// TestMigrate_APanicIsExit2: install-core.sh reads exit 1 as a refusal (the
// database is one this build will not start on), so a panic inside migrate
// exits 2 with the panic named, not 1 through main's recover. The panic here
// is the first write of the usage.
func TestMigrate_APanicIsExit2(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	stderr := &panicOnFirstWrite{}
	if code := runMigrate(nil, &stdout, stderr); code != 2 || !strings.Contains(stderr.String(), "migrate: panic: the writer broke") {
		t.Errorf("a panic in migrate: exit %d stderr %q, want exit 2 and the panic named", code, stderr.String())
	}
}

// TestMigrate_AConfigThatIsNotThereIsExit2: config.Load reads a missing file
// as the defaults, a database on localhost, so `migrate down` run from the
// wrong directory (the -config default is relative) stepped whatever answered
// there. A config file that is not there exits 2, naming it, before any
// database.
func TestMigrate_AConfigThatIsNotThereIsExit2(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "shingocore.yaml")
	for _, args := range [][]string{
		{"-config", missing, "check"},
		{"-config", missing, "down", "185"},
	} {
		var stdout, stderr bytes.Buffer
		code := runMigrate(args, &stdout, &stderr)
		if code != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "config "+missing) {
			t.Errorf("migrate %q: exit %d stdout %q stderr %q, want exit 2 naming %s", args, code, stdout.String(), stderr.String(), missing)
		}
	}
}

// TestMain_AnArgumentAfterTheFlagsIsUsage: Core reads flags only, so an
// argument left after them is a usage error: exit 2 and the usage on stderr,
// before the boot's first line. `shingocore -config X migrate down 185`, the
// migrate command written after a flag, started a live Core on X's database;
// so did a debug filter written as a second word (--log-debug rds). main
// exits the process, so it runs in a child: this test binary again, told by
// the environment to call main with the arguments, in a scratch directory.
func TestMain_AnArgumentAfterTheFlagsIsUsage(t *testing.T) {
	if args, ok := os.LookupEnv("SHINGOCORE_TEST_MAIN_ARGS"); ok {
		os.Args = append([]string{"shingocore"}, strings.Split(args, "\x1f")...)
		main()
		os.Exit(0)
	}
	t.Parallel()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// A database nothing answers on: a child that boots stops at the open.
	cfg := filepath.Join(dir, "shingocore.yaml")
	if err := os.WriteFile(cfg, []byte("database:\n  postgres:\n    host: 127.0.0.1\n    port: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-config", cfg, "migrate", "down", "185"},
		{"--config", cfg, "check"},
		{"--log-debug", "rds", "--config", cfg},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		cmd := exec.CommandContext(ctx, self, "-test.run=^TestMain_AnArgumentAfterTheFlagsIsUsage$")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "SHINGOCORE_TEST_MAIN_ARGS="+strings.Join(args, "\x1f"))
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		cancel()
		code := 0
		if exit := (*exec.ExitError)(nil); errors.As(err, &exit) {
			code = exit.ExitCode()
		} else if err != nil {
			t.Fatalf("run shingocore %q: %v", args, err)
		}
		if code != 2 || !strings.Contains(stderr.String(), "is not a flag") || !strings.Contains(stderr.String(), "Usage: shingocore") {
			t.Errorf("shingocore %q: exit %d, stderr %q; want exit 2, the argument named and the usage on stderr", args, code, stderr.String())
		}
		if strings.Contains(stderr.String(), "shingocore: starting") {
			t.Errorf("shingocore %q started a boot", args)
		}
	}
}

// TestInstallCore_AFailedBackupStopsTheInstall runs install-core.sh's backup
// block: a backup tar cannot write exits 1 naming it, and a first install,
// with nothing to back up, goes on.
func TestInstallCore_AFailedBackupStopsTheInstall(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the installer is a Linux script")
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "install-core.sh"))
	if err != nil {
		t.Fatal(err)
	}
	const opens = `if [ ${#BACKUP_FILES[@]} -gt 0 ]; then`
	_, rest, found := strings.Cut(string(raw), opens)
	block, _, closed := strings.Cut(rest, "\nfi\n")
	if !found || !closed {
		t.Fatal("install-core.sh has no backup block: move this pin with it")
	}
	dir := t.TempDir()
	config := filepath.Join(dir, "shingocore.yaml")
	if err := os.WriteFile(config, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "set -euo pipefail\nBACKUP_PATH=$1; shift; BACKUP_FILES=(\"$@\")\n" + opens + block + "\nfi\necho went-on\n"
	run := func(args ...string) (string, error) {
		out, err := exec.Command("bash", append([]string{"-c", script, "backup"}, args...)...).CombinedOutput()
		return string(out), err
	}
	if out, err := run(filepath.Join(dir, "absent", "b.tgz"), config); err == nil || !strings.Contains(out, "could not back up") || strings.Contains(out, "went-on") {
		t.Errorf("a backup tar cannot write: err %v, output %q; want a failure naming it", err, out)
	}
	if out, err := run(filepath.Join(dir, "b.tgz")); err != nil || !strings.Contains(out, "went-on") {
		t.Errorf("a first install: err %v, output %q; want it to go on", err, out)
	}
}
