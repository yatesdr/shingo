package schemadump

import (
	"context"
	"crypto/sha256"
	_ "embed" // releaseRecipe
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// A ReleaseCache keeps the boot binaries BuildRelease makes across processes,
// so a run whose releases an earlier run built copies verified files instead
// of unpacking a tree and linking a binary per release.
//
// WHY. A release's boot path is a git archive of three modules unpacked into a
// fresh directory, then a go build that GOCACHE serves except for the link. On
// the Windows dev host that measured about 8 s of unpacking and 2 s of link a
// release, 10.7 s of wall for the five in parallel (2026-11-23, the gate-fast
// schemadump lane's probe P1), for inputs that do not change between runs.
//
// THE KEY NAMES EVERY INPUT THE BUILD READS (releaseKey). An input it does not
// name is a binary served for a build that would have made another one; a
// miss costs one build. Nothing in the key is a worktree or a clone, so one
// cache serves every worktree on the host.
//
// NOTHING IS SERVED UNVERIFIED. An entry is the binary and, beside it, a
// manifest holding the key's text and the binary's sha256, each written to a
// temporary file and renamed into place, binary first. A reader copies the
// binary into its own workDir while hashing it, and keeps the copy only when
// the manifest names this key and the hash is the manifest's: a torn write, a
// planted manifest and a half-replaced entry are each a miss
// (TestReleaseCache_ServesOnlyAVerifiedEntry).
type ReleaseCache struct {
	// Dir holds the entries; empty is no cache.
	Dir string
	// Rebuild, when set, says why this process must not read an entry
	// (certification, GitHub). Every release is built; an entry under the
	// same key that holds a different binary fails the build, because the
	// key then misses an input; the entry is replaced.
	Rebuild string
}

var (
	releaseCacheMu sync.Mutex
	releaseCache   ReleaseCache
)

// UseReleaseCache sets the cache BuildRelease uses for the rest of the
// process; call it before the first build. The default, the zero
// ReleaseCache, keeps nothing: the generator commands build every time.
func UseReleaseCache(c ReleaseCache) {
	releaseCacheMu.Lock()
	defer releaseCacheMu.Unlock()
	releaseCache = c
}

func currentReleaseCache() ReleaseCache {
	releaseCacheMu.Lock()
	defer releaseCacheMu.Unlock()
	return releaseCache
}

// releaseRecipe is release.go as compiled into this binary: bootMain,
// bootCmd, releaseModules, the archive, the unpacking, the go build argv and
// buildEnv all live there.
//
//go:embed release.go
var releaseRecipe string

// releaseKey is what a release's boot binary is a function of: one field per
// input of buildBoot that can differ between two builds.
//
// Not in it, and why that is safe:
//   - workDir and where the tree is unpacked: -trimpath keeps paths out of the
//     binary and -buildvcs=false keeps a surrounding checkout out.
//   - GOCACHE: Go's cache changes how long a build takes, not what it makes.
//   - GOPROXY, GOMODCACHE and the other fetch settings: the commit's go.sum
//     pins every dependency's content and go checks a download against it.
//
// Trusted, not keyed: a module cache or a GOROOT edited in place after
// download, under an unchanged go.sum or version. A fresh build would read
// the edit and an entry would not. Certification and GitHub never read an
// entry.
type releaseKey struct {
	// Recipe is the sha256 of releaseRecipe. Any edit to release.go, comments
	// included, is a new key.
	Recipe string
	// Commit is the commit id the archive is of: the three modules' source,
	// the release's go.mod and go.sum, and its in-tree .gitattributes.
	Commit string
	// Git is `git version`: git archive's text detection and conversions are
	// git's own.
	Git string
	// GitAttributes is the sha256 of each attributes file git reads beside
	// the commit's (system, global, the clone's info/attributes), or
	// "absent". Any of them can change a file's bytes in the archive: eol,
	// filter, export-ignore, export-subst.
	GitAttributes string
	// GitConfig is the sha256 of the config the archive's conversions read
	// where the attributes leave a choice: core.autocrlf, core.eol,
	// core.attributesfile (git config keys ignore case) and the filter drivers.
	GitConfig string
	// GoEnv is `go env` of the variables that shape a binary, read in
	// repoRoot under buildEnv, as the build sees them (goEnvKeys).
	GoEnv string
}

// goEnvKeys are the go env variables a pure-Go build's output depends on:
// the toolchain (GOVERSION, GOROOT, and GOTOOLCHAIN, which with the commit's
// go.mod decides any switch), the target and each architecture's level,
// GOEXPERIMENT, GOFIPS140, GOFLAGS (a `go env -w` value survives buildEnv's
// empty one) and CGO_ENABLED.
var goEnvKeys = []string{
	"GOVERSION", "GOROOT", "GOTOOLCHAIN", "GOOS", "GOARCH",
	"GOAMD64", "GOARM", "GOARM64", "GO386", "GOMIPS", "GOMIPS64", "GOPPC64", "GORISCV64", "GOWASM",
	"GOEXPERIMENT", "GOFIPS140", "GOFLAGS", "CGO_ENABLED",
}

// releaseKeyVersion heads every key's text: a change to what a key holds or
// how an entry is laid out bumps it.
const releaseKeyVersion = "schemadump release boot cache v1"

// text is the key as the manifest records it, one input a line.
func (k releaseKey) text() string {
	return releaseKeyVersion + "\n" +
		"recipe " + k.Recipe + "\n" +
		"commit " + k.Commit + "\n" +
		"git " + k.Git + "\n" +
		"git-attributes " + k.GitAttributes + "\n" +
		"git-config " + k.GitConfig + "\n" +
		"go " + k.GoEnv + "\n"
}

// entryName is the entry's file name in the cache: the release, then the
// key's hash.
func entryName(rev string, k releaseKey) string {
	return "boot-" + safeName(rev) + "-" + sha256Hex([]byte(k.text()))[:16]
}

var (
	hostKeysMu sync.Mutex
	hostKeys   = map[string]*hostKey{}
)

// hostKey is every field of a releaseKey but Commit: the same for each
// release a process builds from one clone, so it is read once.
type hostKey struct {
	once sync.Once
	key  releaseKey
	err  error
}

// releaseKeyFor is the key of commit's boot binary built from repoRoot's
// clone on this host.
func releaseKeyFor(ctx context.Context, repoRoot, commit string) (releaseKey, error) {
	hostKeysMu.Lock()
	h, ok := hostKeys[repoRoot]
	if !ok {
		h = &hostKey{}
		hostKeys[repoRoot] = h
	}
	hostKeysMu.Unlock()
	h.once.Do(func() { h.key, h.err = readHostKey(ctx, repoRoot) })
	if h.err != nil {
		return releaseKey{}, h.err
	}
	k := h.key
	k.Commit = commit
	return k, nil
}

func readHostKey(ctx context.Context, repoRoot string) (releaseKey, error) {
	run := func(env []string, name string, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = repoRoot
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.Output()
		return string(out), err
	}
	k := releaseKey{Recipe: sha256Hex([]byte(releaseRecipe))}

	v, err := run(nil, "git", "version")
	if err != nil {
		return k, fmt.Errorf("git version: %w", err)
	}
	k.Git = strings.TrimSpace(v)

	var attrs []string
	for _, q := range []struct {
		label string
		args  []string
	}{
		{"system", []string{"var", "GIT_ATTR_SYSTEM"}},
		{"global", []string{"var", "GIT_ATTR_GLOBAL"}},
		{"info", []string{"rev-parse", "--git-path", "info/attributes"}},
	} {
		p, err := run(nil, "git", q.args...)
		if err != nil {
			return k, fmt.Errorf("git %s: %w", strings.Join(q.args, " "), err)
		}
		p = filepath.FromSlash(strings.TrimSpace(p))
		if !filepath.IsAbs(p) {
			p = filepath.Join(repoRoot, p)
		}
		sum, err := fileSHA256(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			sum = "absent"
		case err != nil:
			return k, fmt.Errorf("read the %s git attributes: %w", q.label, err)
		}
		attrs = append(attrs, q.label+"="+sum)
	}
	k.GitAttributes = strings.Join(attrs, " ")

	conf, err := run(nil, "git", "config", "--get-regexp", `^(core\.(autocrlf|eol|attributesfile)|filter\.)`)
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1 && conf == "") {
		// Exit 1 with no output is "no such key", which is an answer.
		return k, fmt.Errorf("git config: %w", err)
	}
	k.GitConfig = sha256Hex([]byte(conf))

	env, err := run(buildEnv, "go", append([]string{"env"}, goEnvKeys...)...)
	if err != nil {
		return k, fmt.Errorf("go env: %w", err)
	}
	vals := strings.Split(strings.TrimRight(strings.ReplaceAll(env, "\r\n", "\n"), "\n"), "\n")
	if len(vals) != len(goEnvKeys) {
		return k, fmt.Errorf("go env printed %d lines for %d variables", len(vals), len(goEnvKeys))
	}
	pairs := make([]string, len(goEnvKeys))
	for i, name := range goEnvKeys {
		pairs[i] = name + "=" + vals[i]
	}
	k.GoEnv = strings.Join(pairs, " ")
	return k, nil
}

// sumLine prefixes the binary's sha256 in a manifest, after the key's text.
const sumLine = "binary-sha256 "

func (c ReleaseCache) paths(name string) (bin, manifest string) {
	return filepath.Join(c.Dir, name+exeSuffix()), filepath.Join(c.Dir, name+".key")
}

// obtain puts the binary for keyText at dst, from the entry called name when
// it may and that entry verifies, else by build (which writes dst). It
// returns how the binary was had ("cached" or "rebuilt (<why>)") and its
// sha256. A failure to store a fresh build is said in the source, not
// returned: the run has its binary either way.
func (c ReleaseCache) obtain(name, keyText, dst string, build func() error) (string, string, error) {
	why := "forced: " + c.Rebuild
	if c.Rebuild == "" {
		sum, miss := c.load(name, keyText, dst)
		if miss == "" {
			return "cached", sum, nil
		}
		why = miss
	}
	if err := build(); err != nil {
		return "", "", err
	}
	sum, err := fileSHA256(dst)
	if err != nil {
		return "", "", err
	}
	source := "rebuilt (" + why + ")"
	var differed error
	if c.Rebuild != "" {
		// The audit only a rebuild can make: under one key, a fresh build and
		// the entry a run would have been served must be the same binary.
		binPath, keyPath := c.paths(name)
		if was, miss := manifestSum(keyPath, keyText); miss == "" && was != sum {
			kept := strings.TrimSuffix(binPath, exeSuffix()) + ".differed" + exeSuffix()
			_ = os.Rename(binPath, kept)
			differed = fmt.Errorf("a fresh build differs from the cache entry under the same key (the entry's "+
				"sha256 %s, the fresh build's %s), so the key misses an input the build reads and a run that "+
				"used the entry booted another binary: compare the two (`go version -m` on %s and the new "+
				"entry %s), add the input to releaseKey (release_cache.go), and delete %s", was, sum, kept, binPath, c.Dir)
		}
	}
	if err := c.store(name, keyText, dst, sum); err != nil {
		source += "; not stored: " + err.Error()
	}
	return source, sum, differed
}

// load copies the entry called name to dst and returns its sha256, or why it
// is a miss, leaving no dst.
func (c ReleaseCache) load(name, keyText, dst string) (sum, miss string) {
	binPath, keyPath := c.paths(name)
	want, miss := manifestSum(keyPath, keyText)
	if miss != "" {
		return "", miss
	}
	src, err := os.Open(binPath)
	if errors.Is(err, fs.ErrNotExist) {
		return "", "entry refused: no binary beside its manifest"
	}
	if err != nil {
		return "", "entry unreadable: " + err.Error()
	}
	defer src.Close()
	got, err := copyHashing(dst, src)
	if err != nil {
		_ = os.Remove(dst)
		return "", "entry unreadable: " + err.Error()
	}
	if got != want {
		_ = os.Remove(dst)
		return "", "entry refused: its binary's sha256 is " + got + ", its manifest says " + want
	}
	return got, ""
}

// manifestSum is the binary sha256 the manifest at path records for keyText,
// or why the manifest is not that key's.
func manifestSum(path, keyText string) (sum, miss string) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", "no entry"
	}
	if err != nil {
		return "", "entry unreadable: " + err.Error()
	}
	rest, ok := strings.CutPrefix(string(b), keyText)
	if !ok {
		return "", "entry refused: its manifest names another key"
	}
	s, ok := strings.CutPrefix(rest, sumLine)
	s, nl := strings.CutSuffix(s, "\n")
	if _, err := hex.DecodeString(s); !ok || !nl || len(s) != sha256.Size*2 || err != nil {
		return "", "entry refused: its manifest's sha256 line is malformed"
	}
	return s, ""
}

// store writes src as the entry called name: the binary, then the manifest,
// each by writeRenamed, so a reader that finds a manifest finds the whole
// binary it was written for or a later one, which its hash then refuses.
func (c ReleaseCache) store(name, keyText, src, sum string) error {
	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		return err
	}
	binPath, keyPath := c.paths(name)
	if err := writeRenamed(binPath, func(w io.Writer) error {
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(w, f)
		return err
	}); err != nil {
		return err
	}
	return writeRenamed(keyPath, func(w io.Writer) error {
		_, err := io.WriteString(w, keyText+sumLine+sum+"\n")
		return err
	})
}

// writeRenamed writes path through a temporary file beside it and a rename,
// so a reader sees the old file or the whole new one, never part of either.
// A writer killed before the rename leaves only the temporary file, which no
// reader opens.
func writeRenamed(path string, fill func(io.Writer) error) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(f.Name())
		}
	}()
	if err = fill(f); err == nil {
		err = f.Sync()
	}
	if cErr := f.Close(); err == nil {
		err = cErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// copyHashing copies src to a new executable file at dst and returns the
// sha256 of the bytes written.
func copyHashing(dst string, src io.Reader) (string, error) {
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), src)
	if cErr := f.Close(); err == nil {
		err = cErr
	}
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
