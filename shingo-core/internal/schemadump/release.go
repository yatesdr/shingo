package schemadump

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// A Release is the boot path of a released Core, built from git: that tree's
// own store.Open and nothing else. It is how a database is made the way a
// plant's was, by the code the plant ran, now that this tree no longer
// carries the chain that release replayed.
type Release struct {
	Rev string // the git revision the binary was built from
	bin string
}

// bootMain is the program BuildRelease writes into the release's tree. It runs
// exactly what Core runs at boot before anything else (store.Open, which
// migrates), prints BOOT=OK or BOOT=FAIL with the error, and exits 0 or 1. It
// uses only names every release since v0.11.0 declares: config.DatabaseConfig,
// config.PostgresConfig and store.Open.
const bootMain = `package main

import (
	"flag"
	"fmt"
	"os"

	"shingocore/config"
	"shingocore/store"
)

func main() {
	host := flag.String("host", "", "postgres host")
	port := flag.Int("port", 0, "postgres port")
	db := flag.String("db", "", "database name")
	user := flag.String("user", "", "database user")
	password := flag.String("password", "", "database password")
	flag.Parse()
	d, err := store.Open(&config.DatabaseConfig{Postgres: config.PostgresConfig{
		Host: *host, Port: *port, Database: *db,
		User: *user, Password: *password, SSLMode: "disable",
	}})
	if err != nil {
		fmt.Println("BOOT=FAIL", err)
		os.Exit(1)
	}
	d.Close()
	fmt.Println("BOOT=OK")
}
`

// bootCmd is where bootMain is written inside the release's shingo-core
// module. No release has a command of this name.
const bootCmd = "cmd/zzschemaboot"

// releaseModules are the modules a Core binary is built from; shingo-core's
// go.mod reaches the other two by relative replace.
var releaseModules = []string{"protocol", "shared", "shingo-core"}

var (
	releaseMu    sync.Mutex
	releaseBuilt = map[string]*releaseBuild{}
)

type releaseBuild struct {
	once sync.Once
	rel  *Release
	err  error
}

// BuildRelease builds rev's boot path into workDir, once per process per
// (workDir, rev): git archive of the three modules into workDir, bootMain
// written into that tree, go build there. repoRoot is any directory inside the
// work tree. The caller owns workDir and removes it.
//
// The build runs with GOWORK=off (the archive's go.work names modules it does
// not carry), CGO_ENABLED=0 (Core is pure Go, and the Windows host has no C
// compiler), GOFLAGS cleared (a caller's -tags must not reach a release's
// tree) and -trimpath, so the Go build cache is shared between builds of the
// same source in different directories.
func BuildRelease(ctx context.Context, repoRoot, rev, workDir string) (*Release, error) {
	key := workDir + "\x00" + rev
	releaseMu.Lock()
	b, ok := releaseBuilt[key]
	if !ok {
		b = &releaseBuild{}
		releaseBuilt[key] = b
	}
	releaseMu.Unlock()
	b.once.Do(func() { b.rel, b.err = buildRelease(ctx, repoRoot, rev, workDir) })
	return b.rel, b.err
}

func buildRelease(ctx context.Context, repoRoot, rev, workDir string) (*Release, error) {
	if !revisionExists(repoRoot, rev) {
		return nil, errors.New(missingRevisionHint(rev))
	}
	tree := filepath.Join(workDir, "src-"+safeName(rev))
	if err := os.RemoveAll(tree); err != nil {
		return nil, fmt.Errorf("clear %s: %w", tree, err)
	}
	if err := archiveInto(ctx, repoRoot, rev, tree); err != nil {
		return nil, err
	}
	mainDir := filepath.Join(tree, "shingo-core", filepath.FromSlash(bootCmd))
	if err := os.MkdirAll(mainDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(mainDir, "main.go"), []byte(bootMain), 0o644); err != nil {
		return nil, err
	}
	bin := filepath.Join(workDir, "boot-"+safeName(rev)+exeSuffix())
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", bin, "./"+bootCmd)
	cmd.Dir = filepath.Join(tree, "shingo-core")
	cmd.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0", "GOFLAGS=")
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("go build %s's boot path: %w\n%s", rev, err, out)
	}
	return &Release{Rev: rev, bin: bin}, nil
}

// archiveInto writes rev's copy of the release modules under dir, read from
// git archive's tar stream. Only regular files and directories are written;
// the pax header git puts first is skipped by the reader.
func archiveInto(ctx context.Context, repoRoot, rev, dir string) error {
	// git archive reads paths relative to the directory it runs in, so it runs
	// at the top of the work tree.
	top, err := exec.CommandContext(ctx, "git", "-C", repoRoot, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return fmt.Errorf("find the work tree's top from %s: %w", repoRoot, err)
	}
	args := append([]string{"-C", strings.TrimSpace(string(top)), "archive", "--format=tar", rev}, releaseModules...)
	cmd := exec.CommandContext(ctx, "git", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("git archive %s: %w", rev, err)
	}
	extractErr := extractTar(out, dir)
	_, _ = io.Copy(io.Discard, out)
	waitErr := cmd.Wait()
	if extractErr != nil {
		return fmt.Errorf("unpack git archive %s: %w", rev, extractErr)
	}
	if waitErr != nil {
		return fmt.Errorf("git archive %s: %w: %s", rev, waitErr, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func extractTar(r io.Reader, dir string) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.FromSlash(h.Name)
		if !filepath.IsLocal(name) {
			return fmt.Errorf("archive entry %q leaves the tree", h.Name)
		}
		p := filepath.Join(dir, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			_, cErr := io.Copy(f, tr)
			if err := f.Close(); cErr == nil {
				cErr = err
			}
			if cErr != nil {
				return cErr
			}
		}
	}
}

// BootRelease runs rel's store.Open against dbName on this instance and
// returns what it printed. A boot that fails is an error carrying that output.
func (i *Instance) BootRelease(ctx context.Context, rel *Release, dbName string) (string, error) {
	cmd := exec.CommandContext(ctx, rel.bin,
		"-host", i.host, "-port", fmt.Sprint(i.port), "-db", dbName,
		"-user", pgUser, "-password", pgPass)
	out, err := cmd.CombinedOutput()
	if err != nil || !bytes.Contains(out, []byte("BOOT=OK")) {
		return string(out), fmt.Errorf("%s's store.Open on %s: %v\n%s", rel.Rev, dbName, err, out)
	}
	return string(out), nil
}

func safeName(rev string) string {
	return strings.NewReplacer("^", "-parent", "/", "-", ".", "_", "~", "-").Replace(rev)
}

func exeSuffix() string {
	if filepath.Separator == '\\' {
		return ".exe"
	}
	return ""
}
