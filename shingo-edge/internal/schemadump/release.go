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

// A Release is the boot path of a released Edge, built from git: that tree's
// own store.Open, and the three reads a board lives on. It is how a database
// is made the way a plant's was, now that this tree no longer carries the
// chain that release ran, and how that release is shown to run on what this
// tree leaves. The shape is Core's (shingo-core/internal/schemadump), on a
// file in place of a Postgres.
type Release struct {
	Rev string // the git revision the binary was built from
	bin string
}

// bootMain is the program BuildRelease writes into the release's tree. It runs
// what an Edge runs at boot before anything else (store.Open, which migrates),
// reads the active orders, the processes and their claims, prints BOOT=OK or
// BOOT=FAIL with the error, and exits 0 or 1. It uses only names
// store.RetiredChainRelease declares.
const bootMain = `package main

import (
	"fmt"
	"os"

	"shingoedge/store"
)

func fail(err error) {
	fmt.Println("BOOT=FAIL", err)
	os.Exit(1)
}

func main() {
	d, err := store.Open(os.Args[1])
	if err != nil {
		fail(err)
	}
	defer d.Close()
	if _, err := d.ListActiveOrders(); err != nil {
		fail(fmt.Errorf("read the orders: %w", err))
	}
	ps, err := d.ListProcesses()
	if err != nil {
		fail(fmt.Errorf("read the processes: %w", err))
	}
	for _, p := range ps {
		ss, err := d.ListStylesByProcess(p.ID)
		if err != nil {
			fail(fmt.Errorf("read the styles: %w", err))
		}
		for _, s := range ss {
			if _, err := d.ListStyleNodeClaims(s.ID); err != nil {
				fail(fmt.Errorf("read the claims: %w", err))
			}
		}
	}
	fmt.Println("BOOT=OK")
}
`

// bootCmd is where bootMain is written inside the release's shingo-edge
// module. No release has a command of this name.
const bootCmd = "cmd/zzschemaboot"

// releaseModules are the modules an Edge binary is built from; shingo-edge's
// go.mod reaches the other two by relative replace.
var releaseModules = []string{"protocol", "shared", "shingo-edge"}

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
// not carry), CGO_ENABLED=0 (the Edge is pure Go, and the Windows host has no C
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
	mainDir := filepath.Join(tree, "shingo-edge", filepath.FromSlash(bootCmd))
	if err := os.MkdirAll(mainDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(mainDir, "main.go"), []byte(bootMain), 0o644); err != nil {
		return nil, err
	}
	bin := filepath.Join(workDir, "boot-"+safeName(rev)+exeSuffix())
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", bin, "./"+bootCmd)
	cmd.Dir = filepath.Join(tree, "shingo-edge")
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

// Boot runs the release's store.Open and reads on the file at path and returns
// what it printed. A boot that fails is an error carrying that output.
func (r *Release) Boot(ctx context.Context, path string) (string, error) {
	out, err := exec.CommandContext(ctx, r.bin, path).CombinedOutput()
	if err != nil || !bytes.Contains(out, []byte("BOOT=OK")) {
		return string(out), fmt.Errorf("%s's store.Open on %s: %v\n%s", r.Rev, path, err, out)
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
