package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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
