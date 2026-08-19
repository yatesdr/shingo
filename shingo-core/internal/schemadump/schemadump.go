// Package schemadump builds shingo-core's Postgres schema in a throwaway
// container and dumps it, so the shape can be committed as a file and
// compared.
//
// WHY THIS EXISTS. There is no file in this repository you can open to learn
// what the database looks like. The real shape is postgres_ddl.go plus
// migrateRenames() plus migrateAddBaselineColumns() plus every versioned
// migration plus HOW OLD THAT PARTICULAR DATABASE IS.
//
// That last term is the one that bites. schema.Apply runs CREATE ... IF NOT
// EXISTS, which does NOTHING to a table that already exists — so a column
// added to a baseline CREATE TABLE reaches a fresh database instantly and a
// plant database never. migrateAddBaselineColumns exists to patch that, and
// it is a hand-maintained list whose own comment states the rule with nothing
// enforcing it. It has failed twice: the misplaced code/ref index (worked on a
// fresh DB, absent at the plant) and the five long-inert baseline indexes an
// old dump trips.
//
// So this package builds the schema BOTH ways — today's baseline, and an old
// baseline pulled out of git — and the tests assert they converge. Anything
// that only works on a fresh database fails on the machine of whoever wrote
// it, instead of at a plant.
package schemadump

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/docker/go-connections/nat"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"database/sql"

	"shingocore/config"
	"shingocore/store"
)

// SnapshotPath is where the committed dump lives, relative to the shingo-core
// module root. Named here so the generator, the staleness test and the failure
// message all say the same thing.
const SnapshotPath = "store/schema/schema.snapshot.sql"

// RegenCommand is the exact command the staleness test tells you to run. It
// belongs beside the path for the same reason.
const RegenCommand = "make schema-snapshot"

const (
	pgImage = "postgres:16-alpine"
	pgUser  = "test"
	pgPass  = "test"
)

// Instance is a running throwaway Postgres.
type Instance struct {
	container testcontainers.Container
	host      string
	port      int
}

// Start boots a Postgres container. The caller must Close it.
//
// The container runs with the same durability-off tuning as the gate's shared
// server (scripts/gate.sh start_shared_pg): fsync/synchronous_commit/
// full_page_writes off, data on tmpfs. Nothing here outlives the process —
// the whole point is a shape to diff — and with default settings the
// migration path's thousands of DDL fsyncs are this package's dominant cost
// under co-tenancy.
func Start(ctx context.Context) (*Instance, error) {
	c, err := postgres.Run(ctx, pgImage,
		postgres.WithDatabase("postgres"),
		postgres.WithUsername(pgUser),
		postgres.WithPassword(pgPass),
		testcontainers.WithEnv(map[string]string{"PGDATA": "/var/lib/postgresql/data/pgdata"}),
		testcontainers.CustomizeRequest(testcontainers.GenericContainerRequest{
			ContainerRequest: testcontainers.ContainerRequest{
				Cmd: []string{
					"-c", "fsync=off",
					"-c", "synchronous_commit=off",
					"-c", "full_page_writes=off",
				},
				Tmpfs: map[string]string{"/var/lib/postgresql/data": "rw,size=1g"},
			},
		}),
		testcontainers.WithWaitStrategy(wait.ForAll(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(90*time.Second),
			wait.ForListeningPort(nat.Port("5432/tcp")).
				WithStartupTimeout(90*time.Second),
		)),
	)
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", pgImage, err)
	}
	host, err := c.Host(ctx)
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, fmt.Errorf("container host: %w", err)
	}
	port, err := c.MappedPort(ctx, "5432")
	if err != nil || port.Int() == 0 {
		_ = c.Terminate(ctx)
		return nil, fmt.Errorf("container port (err=%v val=%d)", err, port.Int())
	}
	return &Instance{container: c, host: host, port: port.Int()}, nil
}

// Close terminates the container.
func (i *Instance) Close(ctx context.Context) error {
	if i == nil || i.container == nil {
		return nil
	}
	return i.container.Terminate(ctx)
}

func (i *Instance) admin() (*sql.DB, error) {
	return sql.Open("pgx", fmt.Sprintf(
		"host=%s port=%d dbname=postgres user=%s password=%s sslmode=disable",
		i.host, i.port, pgUser, pgPass))
}

func (i *Instance) dbConfig(name string) *config.DatabaseConfig {
	return &config.DatabaseConfig{Postgres: config.PostgresConfig{
		Host: i.host, Port: i.port, Database: name,
		User: pgUser, Password: pgPass, SSLMode: "disable",
	}}
}

// createDatabase makes an empty database with a unique name.
func (i *Instance) createDatabase(prefix string) (string, error) {
	name := fmt.Sprintf("%s_%d", prefix, rand.Intn(1_000_000))
	admin, err := i.admin()
	if err != nil {
		return "", fmt.Errorf("admin connection: %w", err)
	}
	defer admin.Close()
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		return "", fmt.Errorf("create database %s: %w", name, err)
	}
	return name, nil
}

// BuildFresh creates an empty database and runs the FULL production migrate
// path against it — renames, pre-baseline column adds, today's baseline, then
// every versioned migration. This is what a brand-new plant gets.
func (i *Instance) BuildFresh(_ context.Context) (string, error) {
	name, err := i.createDatabase("fresh")
	if err != nil {
		return "", err
	}
	db, err := store.Open(i.dbConfig(name))
	if err != nil {
		return "", fmt.Errorf("migrate fresh database: %w", err)
	}
	db.Close()
	return name, nil
}

// BuildAged creates an empty database, applies an OLD baseline DDL to it, and
// then runs the full production migrate path — which is exactly what happens
// when a plant that was installed at that vintage takes an upgrade.
//
// The old baseline is applied raw, without the current migrateRenames /
// migrateAddBaselineColumns running first, because at that vintage those steps
// either did not exist or had different contents. What the upgrade path
// actually has to survive is "this shape, then today's code", and that is what
// this builds.
func (i *Instance) BuildAged(_ context.Context, baselineSQL string) (string, error) {
	if strings.TrimSpace(baselineSQL) == "" {
		return "", fmt.Errorf("empty baseline DDL")
	}
	name, err := i.createDatabase("aged")
	if err != nil {
		return "", err
	}
	aged, err := sql.Open("pgx", fmt.Sprintf(
		"host=%s port=%d dbname=%s user=%s password=%s sslmode=disable",
		i.host, i.port, name, pgUser, pgPass))
	if err != nil {
		return "", fmt.Errorf("connect to %s: %w", name, err)
	}
	_, execErr := aged.Exec(baselineSQL)
	aged.Close()
	if execErr != nil {
		return "", fmt.Errorf("apply old baseline to %s: %w", name, execErr)
	}

	db, err := store.Open(i.dbConfig(name))
	if err != nil {
		return "", fmt.Errorf("migrate aged database: %w", err)
	}
	db.Close()
	return name, nil
}

// dumpFile is where pg_dump writes inside the container before we copy it out.
// Per-database, not fixed: tests dump concurrently against one shared instance,
// and a fixed path made every concurrent dump read whichever run's output
// landed last — the staleness test read a vintage's dump as "fresh".
func dumpFile(dbName string) string { return "/tmp/shingo-schema-" + dbName + ".sql" }

// Dump runs pg_dump --schema-only inside the container and returns the
// normalized result.
//
// pg_dump runs IN the container, not on the host, deliberately: it means this
// works on a Windows dev box with no Postgres client tools installed, and the
// dumping binary always matches the server version.
//
// The output goes to a file and is copied out rather than read from the exec
// stream. Docker multiplexes exec output with an 8-byte binary frame header
// per chunk, which lands at arbitrary offsets in the middle of the SQL; a file
// copy gives clean bytes and the difference is visible the moment you open the
// snapshot.
func (i *Instance) Dump(ctx context.Context, dbName string) (string, error) {
	file := dumpFile(dbName)
	code, reader, err := i.container.Exec(ctx, []string{
		"sh", "-c",
		fmt.Sprintf("pg_dump --schema-only --no-owner --no-privileges --no-comments --username=%s %s > %s",
			pgUser, dbName, file),
	})
	if err != nil {
		return "", fmt.Errorf("exec pg_dump: %w", err)
	}
	if code != 0 {
		var sb strings.Builder
		_, _ = io.Copy(&sb, reader)
		return "", fmt.Errorf("pg_dump exited %d: %s", code, sb.String())
	}

	rc, err := i.container.CopyFileFromContainer(ctx, file)
	if err != nil {
		return "", fmt.Errorf("copy %s out of container: %w", file, err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return "", fmt.Errorf("read dump: %w", err)
	}
	return Normalize(string(body)), nil
}

var (
	// SET/SELECT preamble lines are environment, not shape.
	dumpPreambleLine = regexp.MustCompile(`(?m)^(SET |SELECT pg_catalog\.set_config).*$`)
	// \restrict / \unrestrict carry a RANDOM token, regenerated on every dump.
	// Left in, the snapshot differs from itself on consecutive runs and the
	// staleness test fails permanently on a tree nobody has touched.
	dumpRestrictLine = regexp.MustCompile(`(?m)^\\(un)?restrict\s+\S+\s*$`)
	// Blank-line runs.
	blankRun = regexp.MustCompile(`\n{3,}`)
)

// Normalize strips everything from a pg_dump that is about the DUMP rather
// than about the SHAPE — versions, the source database name, the session
// preamble, comment banners and blank-line runs.
//
// What survives is the schema and only the schema, which is what makes the
// committed snapshot readable as a diff in review and what makes fresh and
// aged comparable at all.
func Normalize(dump string) string {
	s := dump
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = dumpPreambleLine.ReplaceAllString(s, "")
	s = dumpRestrictLine.ReplaceAllString(s, "")

	var kept []string
	for line := range strings.SplitSeq(s, "\n") {
		t := strings.TrimRight(line, " \t")
		// Drop pg_dump's own banner comments. Object-level "-- Name: x; Type:
		// TABLE" headers go too: they restate what the statement beneath them
		// already says, and they carry the schema owner.
		if strings.HasPrefix(t, "--") {
			continue
		}
		kept = append(kept, t)
	}
	s = strings.Join(kept, "\n")
	s = blankRun.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s) + "\n"
}

// Canonical reduces a normalized dump to an ORDER-INSENSITIVE description of
// the shape: statements sorted, and the body of each CREATE TABLE sorted
// within itself.
//
// Order is exactly what legitimately differs between a fresh database and an
// aged one, and it is not part of the shape. On a fresh database a column sits
// wherever the baseline CREATE TABLE puts it; on an upgraded database the same
// column was appended by an ALTER, so it sits at the end. Postgres records
// that as attnum and pg_dump prints attnum order. Nothing about the database
// behaves differently — SQL addresses columns by name.
//
// So the convergence test compares canonical forms. The COMMITTED snapshot
// keeps the real order, because its job is to be read.
//
// The one thing this would hide is a column-order dependency in the
// application: `SELECT *` positional scanning, or INSERT without a column
// list. shingo-core writes neither, and if it ever does, the fix is to stop —
// not to make the convergence test assert an order that plant databases
// cannot have anyway.
func Canonical(dump string) string {
	stmts := splitStatements(dump)
	out := make([]string, 0, len(stmts))
	for _, s := range stmts {
		out = append(out, canonicalStatement(s))
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// splitStatements breaks a dump into statements on semicolon-terminated lines.
// Good enough for pg_dump output, which never puts a bare `;` inside a string
// literal in schema-only mode.
func splitStatements(dump string) []string {
	var stmts []string
	var cur []string
	for line := range strings.SplitSeq(dump, "\n") {
		t := strings.TrimSpace(line)
		if t == "" && len(cur) == 0 {
			continue
		}
		cur = append(cur, line)
		if strings.HasSuffix(t, ";") {
			stmts = append(stmts, strings.TrimSpace(strings.Join(cur, "\n")))
			cur = nil
		}
	}
	if len(cur) > 0 {
		if s := strings.TrimSpace(strings.Join(cur, "\n")); s != "" {
			stmts = append(stmts, s)
		}
	}
	return stmts
}

var createTableHead = regexp.MustCompile(`(?i)^CREATE TABLE [^(]*\($`)

// canonicalStatement sorts the body of a CREATE TABLE and leaves everything
// else alone.
func canonicalStatement(stmt string) string {
	lines := strings.Split(stmt, "\n")
	if len(lines) < 3 || !createTableHead.MatchString(strings.TrimSpace(lines[0])) {
		return stmt
	}
	last := len(lines) - 1
	if strings.TrimSpace(lines[last]) != ");" {
		return stmt
	}
	body := make([]string, 0, last-1)
	for _, l := range lines[1:last] {
		// Trailing commas are positional punctuation, not content: the last
		// column has none and would sort as a different string from the same
		// column in the middle of the list.
		body = append(body, strings.TrimSuffix(strings.TrimSpace(l), ","))
	}
	sort.Strings(body)
	return lines[0] + "\n" + strings.Join(body, "\n") + "\n" + lines[last]
}
