// Package schemadump builds shingo-core's Postgres schema in a throwaway
// container and dumps it, so the shape can be committed as a file and
// compared.
//
// WHY THIS EXISTS. A database's shape depends on how it was made, and Core's
// databases are made two ways: a new one is born on the baseline
// (store/schema/baseline.sql), and a plant's was built by the releases it ran,
// whose replay this tree no longer carries. The baseline is generated here
// (GenerateBaseline: the retired chain's own release, built from git and
// booted, then dumped), and the tests rebuild plant-shaped databases with the
// releases' own binaries (BuildRelease) and assert each one, brought up by
// this build, has the shape of one born on the baseline. Anything that only
// works on a new database fails on the machine of whoever wrote it, instead of
// at a plant.
//
// Until the squash at v184 the shape was the baseline DDL replayed on every
// boot plus the pre-baseline renames and column adds plus every versioned
// migration plus how old the database was; a column added to the baseline
// CREATE TABLE reached a new database and never a plant one. That cost two
// plant deploys, and is what this package was first built to catch.
package schemadump

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
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

// Instance is a running throwaway Postgres: a container Start started, or the
// server $SHINGO_TEST_PG names (Start).
type Instance struct {
	// target is the container pg_dump runs in (pgDump).
	target dumpTarget
	// own is the container Start started, which Close removes; nil on the
	// server $SHINGO_TEST_PG names, which its owner removes.
	own  testcontainers.Container
	host string
	port int
}

// dumpTarget is what pgDump needs of a container: testcontainers' own, or the
// shared server's (serverContainer).
type dumpTarget interface {
	Exec(ctx context.Context, cmd []string, options ...tcexec.ProcessOption) (int, io.Reader, error)
	CopyFileFromContainer(ctx context.Context, filePath string) (io.ReadCloser, error)
}

// Start boots a Postgres container, or, with $SHINGO_TEST_PG set, uses the
// server it names. The caller must Close it.
//
// WITH $SHINGO_TEST_PG SET, NO CONTAINER OF ITS OWN (2026-11-24, brief A2
// R45; pinned by TestStart_UsesTheServerSHINGO_TEST_PGNames). The variable
// names the gate's shared server (scripts/gate.sh start_shared_pg) or CI's
// (.github/actions/shared-postgres), and the rule is testdb's: that server, no
// fallback. On the gate's Linux side, Docker Desktop exposes a container's port
// inside WSL once, when the container starts, by listening on the number the
// host side chose; while the docker step's own connections hold thousands of
// ephemeral ports in TIME-WAIT, a held number makes that listen fail with
// EADDRINUSE and every dial from WSL is refused for the container's life. This
// package's container and its testcontainers reaper start mid-step, and one of
// their two numbers was held in 4 of 12 Linux-side docker steps on the dev
// laptop (2026-11-24). Binding 127.0.0.1 is no cure: testcontainers hands it
// to the engine as HostPort "0", from the same sequential range. The shared
// server's port is exposed as the docker step starts, before the step's own
// connections begin. pg_dump runs in that server's container (startOn), so the
// dump is still that server's version's. Forbids: a container started here while $SHINGO_TEST_PG is set.
// Expiry: Docker Desktop retrying a number it could not bind in WSL, or the
// gate's Linux side running without Docker Desktop.
//
// Its own container runs with the same durability-off tuning as the gate's
// shared server: fsync/synchronous_commit/full_page_writes off, data on tmpfs.
// Nothing here outlives the process — the whole point is a shape to diff — and
// with default settings the migration path's thousands of DDL fsyncs are this
// package's dominant cost under co-tenancy.
func Start(ctx context.Context) (*Instance, error) {
	if addr := os.Getenv("SHINGO_TEST_PG"); addr != "" {
		return startOn(ctx, addr)
	}
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
	return &Instance{target: c, own: c, host: host, port: port.Int()}, nil
}

// startOn is Start on the server at addr (host:port, $SHINGO_TEST_PG): the
// running container that publishes its port runs pg_dump, and nothing is
// started. The container is found by the port as `docker port` reports it,
// which is how the gate computed addr. A server no container publishes is an
// error, not a container of this package's own: pg_dump must be the server's.
func startOn(ctx context.Context, addr string) (*Instance, error) {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("SHINGO_TEST_PG=%q is not host:port: %w", addr, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 {
		return nil, fmt.Errorf("SHINGO_TEST_PG=%q has an unusable port", addr)
	}
	cli, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	running, err := cli.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("list containers: %w", err)
	}
	for _, c := range running {
		for _, p := range c.Ports {
			if int(p.PublicPort) != port || p.PrivatePort != 5432 {
				continue
			}
			if c.Image != pgImage {
				_ = cli.Close()
				return nil, fmt.Errorf("SHINGO_TEST_PG=%s is %s, and the dump must be %s's pg_dump", addr, c.Image, pgImage)
			}
			return &Instance{target: &serverContainer{cli: cli, id: c.ID}, host: host, port: port}, nil
		}
	}
	_ = cli.Close()
	return nil, fmt.Errorf("SHINGO_TEST_PG=%s: no running container publishes port %d, and pg_dump runs in "+
		"the server's container (unset it and schemadump starts its own)", addr, port)
}

// serverContainer is the container of the server $SHINGO_TEST_PG names, for
// pg_dump, through the Docker client testcontainers uses.
type serverContainer struct {
	cli *testcontainers.DockerClient
	id  string
}

// Exec runs cmd in the container and returns its exit code and its output,
// stdout and stderr demultiplexed into one stream: what pgDump asks
// testcontainers for (tcexec.Multiplexed); the options are not read. The
// output is read to its end, which is the process's end, before the code is
// asked for.
func (s *serverContainer) Exec(ctx context.Context, cmd []string, _ ...tcexec.ProcessOption) (int, io.Reader, error) {
	created, err := s.cli.ContainerExecCreate(ctx, s.id, container.ExecOptions{Cmd: cmd, AttachStdout: true, AttachStderr: true})
	if err != nil {
		return 0, nil, fmt.Errorf("container exec create: %w", err)
	}
	attached, err := s.cli.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		return 0, nil, fmt.Errorf("container exec attach: %w", err)
	}
	defer attached.Close()
	var out bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &out, attached.Reader); err != nil {
		return 0, nil, fmt.Errorf("container exec output: %w", err)
	}
	inspected, err := s.cli.ContainerExecInspect(ctx, created.ID)
	if err != nil {
		return 0, nil, fmt.Errorf("container exec inspect: %w", err)
	}
	return inspected.ExitCode, &out, nil
}

// CopyFileFromContainer reads one file out of the container: Docker sends a
// tar stream holding the one entry.
func (s *serverContainer) CopyFileFromContainer(ctx context.Context, filePath string) (io.ReadCloser, error) {
	rc, _, err := s.cli.CopyFromContainer(ctx, s.id, filePath)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(rc)
	if _, err := tr.Next(); err != nil {
		_ = rc.Close()
		return nil, fmt.Errorf("read %s from its copy: %w", filePath, err)
	}
	return struct {
		io.Reader
		io.Closer
	}{tr, rc}, nil
}

// Close removes the container Start started. On the server $SHINGO_TEST_PG
// names it only lets go of the Docker client: the gate and CI remove that
// server, and the databases made on it go with it.
func (i *Instance) Close(ctx context.Context) error {
	if i == nil {
		return nil
	}
	if i.own != nil {
		return i.own.Terminate(ctx)
	}
	if s, ok := i.target.(*serverContainer); ok {
		return s.cli.Close()
	}
	return nil
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

// BuildFresh creates an empty database and runs this build's store.Open on it:
// the database is born on the baseline, then the migrations above the retired
// chain run. This is what a brand-new plant gets.
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

// Reboot runs this build's store.Open against an existing database and closes
// it: the boot a restarted Core makes.
func (i *Instance) Reboot(_ context.Context, dbName string) error {
	db, err := store.Open(i.dbConfig(dbName))
	if err != nil {
		return err
	}
	return db.Close()
}

// CreateDatabase makes an empty database with a unique name on this instance.
func (i *Instance) CreateDatabase(prefix string) (string, error) { return i.createDatabase(prefix) }

// ApplyRaw runs sqlText on dbName as one statement, over a plain connection
// with none of Core's session settings: an old baseline DDL, applied the way
// that release's first boot applied it.
func (i *Instance) ApplyRaw(ctx context.Context, dbName, sqlText string) error {
	if strings.TrimSpace(sqlText) == "" {
		return fmt.Errorf("empty SQL for %s", dbName)
	}
	db, err := sql.Open("pgx", fmt.Sprintf(
		"host=%s port=%d dbname=%s user=%s password=%s sslmode=disable",
		i.host, i.port, dbName, pgUser, pgPass))
	if err != nil {
		return fmt.Errorf("connect to %s: %w", dbName, err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, sqlText); err != nil {
		return fmt.Errorf("apply to %s: %w", dbName, err)
	}
	return nil
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
	body, err := i.pgDump(ctx, dbName, "--schema-only --no-owner --no-privileges")
	if err != nil {
		return "", err
	}
	return Normalize(body), nil
}

// pgDump runs pg_dump with flags inside the container and returns its output
// as written, read back from a file in the container (see Dump for why). env
// is prefixed to the command line (e.g. PGTZ=UTC); empty for none.
func (i *Instance) pgDump(ctx context.Context, dbName, flags string, env ...string) (string, error) {
	file := dumpFile(dbName)
	// THE EXIT CODE IS NOT A COMPLETION SIGNAL, so the command prints its own.
	//
	// testcontainers' Exec creates the exec, attaches, then immediately polls
	// ContainerExecInspect in a loop that breaks on !Running. An exec the daemon
	// has not scheduled yet is also not running, and it inspects as
	// {Running:false, ExitCode:0} — so a slow daemon returns "succeeded" before
	// pg_dump has run at all, and the copy below then fails with a file-not-found
	// that names the symptom and hides the cause.
	//
	// It is invisible on a fast machine and reproducible on a loaded one: this
	// failed only in the -race shards, where the detector's slowdown and CI
	// co-tenancy widen the window, and passed in the plain docker job and on a
	// dev box every time.
	//
	// So success is proved by the sentinel, and the drain that reads it is also
	// the synchronisation: the attached stream does not reach EOF until the exec's
	// process exits, so io.Copy returning means pg_dump is done and the file is
	// closed. Both facts come from the same read. (On the server $SHINGO_TEST_PG
	// names the exec is serverContainer's, which drains the stream before it asks
	// for the code; the sentinel is checked the same way.)
	const doneSentinel = "__SHINGO_DUMP_OK__"
	code, reader, err := i.target.Exec(ctx, []string{
		"sh", "-c",
		fmt.Sprintf("%s pg_dump %s --username=%s %s > %s && test -s %s && echo %s",
			strings.Join(env, " "), flags, pgUser, dbName, file, file, doneSentinel),
	}, tcexec.Multiplexed()) // demuxed: the raw stream frames every chunk with an
	// 8-byte header, which can land inside the sentinel and fail the check for a
	// dump that actually succeeded.
	if err != nil {
		return "", fmt.Errorf("exec pg_dump: %w", err)
	}
	var out strings.Builder
	if _, cErr := io.Copy(&out, reader); cErr != nil {
		return "", fmt.Errorf("read pg_dump exec stream for %s: %w", dbName, cErr)
	}
	if !strings.Contains(out.String(), doneSentinel) {
		return "", fmt.Errorf("pg_dump for %s did not report success (exec exit code %d, which is not "+
			"trustworthy on its own): %s", dbName, code, strings.TrimSpace(out.String()))
	}

	rc, err := i.target.CopyFileFromContainer(ctx, file)
	if err != nil {
		return "", fmt.Errorf("copy %s out of container: %w", file, err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return "", fmt.Errorf("read dump: %w", err)
	}
	return string(body), nil
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

var (
	partitionOfRe = regexp.MustCompile(`^CREATE TABLE public\.(\w+) PARTITION OF `)
	attachRe      = regexp.MustCompile(`ATTACH PARTITION public\.(\w+)`)
)

// ExcludePartitions drops from a normalized dump every statement that names a
// partition child: its CREATE TABLE ... PARTITION OF, its ATTACH, and its
// indexes and constraints. Children are made by the code that writes them, at
// runtime (store/heartbeat, store/robotconfidence), never by a boot, so two
// databases of one shape differ in them by what they last wrote. The parent
// stays. A dump with no child comes back unchanged.
func ExcludePartitions(dump string) string {
	stmts := splitStatements(dump)
	children := map[string]bool{}
	for _, s := range stmts {
		if m := partitionOfRe.FindStringSubmatch(s); m != nil {
			children[m[1]] = true
		}
		for _, m := range attachRe.FindAllStringSubmatch(s, -1) {
			children[m[1]] = true
		}
	}
	if len(children) == 0 {
		return dump
	}
	var kept []string
	for _, s := range stmts {
		if !namesAny(s, children) {
			kept = append(kept, s)
		}
	}
	return strings.Join(kept, "\n\n") + "\n"
}

var qualifiedNameRe = regexp.MustCompile(`public\.(\w+)`)

func namesAny(stmt string, names map[string]bool) bool {
	for _, m := range qualifiedNameRe.FindAllStringSubmatch(stmt, -1) {
		if names[m[1]] {
			return true
		}
	}
	return false
}
