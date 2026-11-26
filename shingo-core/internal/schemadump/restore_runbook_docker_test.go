//go:build docker

package schemadump_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/wait"
)

// olderMinorImage is the last Postgres 16 minor without the 2026-11-12 fix
// that records the pg_attver column option. Pinned by minor on purpose: a
// floating 16 tag moves to a fixed minor and then accepts the option.
const olderMinorImage = "postgres:16.15-alpine"

// attverDump is a plain dump as pg_dump 16.15 writes it from a fixed-minor
// server (16.16, 15.20, ...): synthetic tables, the option on their columns.
const attverDump = `SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
CREATE TABLE public.syn_orders (
    id bigint NOT NULL,
    note text DEFAULT ''::text NOT NULL
);
ALTER TABLE ONLY public.syn_orders ALTER COLUMN id SET (pg_attver=26501928);
ALTER TABLE ONLY public.syn_orders ALTER COLUMN note SET (pg_attver=26518400);
COPY public.syn_orders (id, note) FROM stdin;
1	one
\.
ALTER TABLE ONLY public.syn_orders
    ADD CONSTRAINT syn_orders_pkey PRIMARY KEY (id);
`

// restoreCommand is the first sh block of the runbook section "### Restore".
func restoreCommand(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(moduleRoot(t), "docs", "postgresql-setup.md"))
	if err != nil {
		t.Fatal(err)
	}
	const fence = "```"
	_, section, ok := strings.Cut(string(raw), "### Restore\n")
	if !ok {
		t.Fatal("docs/postgresql-setup.md has no Restore section: move this pin with the runbook")
	}
	_, block, ok := strings.Cut(section, fence+"sh\n")
	if !ok {
		t.Fatal("the Restore section has no sh block: move this pin with the runbook")
	}
	command, _, ok := strings.Cut(block, fence)
	if !ok {
		t.Fatal("the Restore section has an unclosed sh block")
	}
	return command
}

// TestRestoreRunbook_AnOlderMinorFailsByName runs the runbook restore command,
// as written, on a Postgres older than the pg_attver fix: a dump from a fixed
// minor fails, naming the option and the server's minor, and leaves nothing
// half restored; the same dump without the option restores.
func TestRestoreRunbook_AnOlderMinorFailsByName(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	command := restoreCommand(t)
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: olderMinorImage,
			Env: map[string]string{
				"POSTGRES_USER": "shingocore", "POSTGRES_DB": "shingocore", "POSTGRES_HOST_AUTH_METHOD": "trust",
				"PGDATA": "/var/lib/postgresql/data/pgdata",
			},
			Cmd:   []string{"-c", "fsync=off"},
			Tmpfs: map[string]string{"/var/lib/postgresql/data": "rw,size=256m"},
			WaitingFor: wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(90 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start %s: %v", olderMinorImage, err)
	}
	defer func() { _ = c.Terminate(context.Background()) }()
	sh := func(script string) (int, string) {
		t.Helper()
		code, out, err := c.Exec(ctx, []string{"sh", "-c", "cd /tmp && " + script}, tcexec.Multiplexed())
		if err != nil {
			t.Fatalf("exec: %v", err)
		}
		b, err := io.ReadAll(out)
		if err != nil {
			t.Fatalf("read the exec output: %v", err)
		}
		return code, string(b)
	}
	restore := func(dump string) (int, string) {
		t.Helper()
		if err := c.CopyToContainer(ctx, []byte(dump), "/tmp/shingocore_backup.sql", 0o644); err != nil {
			t.Fatalf("copy the dump in: %v", err)
		}
		return sh(command)
	}
	tables := func() string {
		t.Helper()
		_, out := sh(`psql -h localhost -U shingocore -d shingocore -Atc "SELECT count(*) FROM pg_tables WHERE schemaname = 'public'"`)
		return strings.TrimSpace(out)
	}

	code, out := restore(attverDump)
	if code == 0 {
		t.Errorf("the runbook restore exited 0 on a dump this server rejects:\n%s", out)
	}
	for _, want := range []string{"pg_attver", "16.15"} {
		if !strings.Contains(out, want) {
			t.Errorf("the failed restore does not name %q:\n%s", want, out)
		}
	}
	if n := tables(); n != "0" {
		t.Errorf("the failed restore left %s tables, want none", n)
		if _, out := sh(`psql -h localhost -U shingocore -d shingocore -qc "DROP TABLE public.syn_orders"`); out != "" {
			t.Logf("clear the half restore: %s", out)
		}
	}

	var kept []string
	for _, line := range strings.Split(attverDump, "\n") {
		if !strings.Contains(line, "pg_attver") {
			kept = append(kept, line)
		}
	}
	if code, out := restore(strings.Join(kept, "\n")); code != 0 {
		t.Errorf("the runbook restore of a dump the server takes exited %d:\n%s", code, out)
	}
	if n := tables(); n != "1" {
		t.Errorf("after the clean restore %s tables, want 1", n)
	}
}
