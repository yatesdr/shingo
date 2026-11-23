//go:build reuseprobe

package probes

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"shingo/shared/scripts/gatetime/reuse"
)

// harness is one run of the probe set.
type harness struct {
	t        *testing.T
	repo     string // the tree under test
	work     string // everything the probes make
	wrapper  string // gatetime, built from the tree under test
	identity string
	real     string // a throwaway copy of the tree (realCopy), made once
	realErr  error
	out      *os.File
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	h.repo = gitOut(t, wd, "rev-parse", "--show-toplevel")
	h.work = os.Getenv("PROBE_WORK")
	if h.work == "" {
		h.work = filepath.Join(os.TempDir(), "gatetime-reuse-probes")
	}
	if err := os.RemoveAll(h.work); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"bin", "synth", "stores"} {
		if err := os.MkdirAll(filepath.Join(h.work, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	h.wrapper = filepath.Join(h.work, "bin", "gatetime"+exeSuffix())
	build := exec.Command("go", "build", "-o", h.wrapper, "./scripts/gatetime")
	build.Dir = filepath.Join(h.repo, "shared")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build gatetime: %v\n%s", err, out)
	}
	h.identity = filepath.Join(h.work, "identity")
	writeFile(t, h.identity, "probe identity\npg_image=sha256:probe\n")
	if h.out, err = os.Create(filepath.Join(h.work, "probe-lines.txt")); err != nil {
		t.Fatal(err)
	}
	return h
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func (h *harness) print(line string) {
	fmt.Println(line)
	fmt.Fprintln(h.out, line)
}

// synth is a small git repo made for one probe: its module, its own store and
// decision log.
type synth struct {
	h        *harness
	dir      string // repo root
	mod      string // module directory
	store    string
	log      string
	identity string
	env      []string
	mode     string
	wrapper  string
	logRead  int
	extra    []string // more go test flags
}

const modFile = "module probe\n\ngo 1.25\n"

// newSynth writes files (paths relative to the repo root) and commits them.
// It waits out the 2 s change-time slack so a first run can be stored.
func (h *harness) newSynth(name string, files map[string]string) *synth {
	dir := filepath.Join(h.work, "synth", name)
	s := &synth{h: h, dir: dir, mod: dir, store: filepath.Join(dir, ".store"), log: filepath.Join(h.work, "synth", name+".log"),
		identity: h.identity, mode: reuse.ModeReuse, wrapper: h.wrapper}
	for p, c := range files {
		writeFile(h.t, filepath.Join(dir, filepath.FromSlash(p)), c)
		if strings.HasSuffix(p, "go.mod") {
			s.mod = filepath.Dir(filepath.Join(dir, filepath.FromSlash(p)))
		}
	}
	writeFile(h.t, filepath.Join(dir, ".gitignore"), ".store/\n*.local\n")
	gitOut(h.t, dir, "init", "-q")
	gitOut(h.t, dir, "add", "-A")
	gitOut(h.t, dir, "-c", "user.email=probe@example.invalid", "-c", "user.name=probe", "commit", "-qm", "probe")
	settle()
	return s
}

// settle waits out reuse's change-time slack (2 s) after an edit, so the next
// run's inputs are older than its start and it can be stored.
func settle() { time.Sleep(2500 * time.Millisecond) }

// step is one go test run and what the wrapper decided for each binary.
type step struct {
	exit int
	out  string
	decs []reuse.Decision
}

func (s *synth) goTest(pkgs ...string) step {
	return s.goTestIn(s.mod, pkgs...)
}

func (s *synth) goTestIn(dir string, pkgs ...string) step {
	args := append([]string{"test", "-count=1", "-exec", "'" + s.wrapper + "' exec"}, s.extra...)
	args = append(args, pkgs...)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GATETIME_REUSE="+s.mode, "GATETIME_STORE="+s.store, "GATETIME_REUSE_LOG="+s.log,
		"GATETIME_IDENTITY="+s.identity)
	if strings.HasPrefix(dir, filepath.Join(s.h.work, "synth")) {
		cmd.Env = append(cmd.Env, "GOWORK=off")
	}
	cmd.Env = append(cmd.Env, s.env...)
	out, err := cmd.CombinedOutput()
	st := step{out: string(out)}
	if ee, ok := err.(*exec.ExitError); ok {
		st.exit = ee.ExitCode()
	} else if err != nil {
		st.exit = -1
		st.out += "\n" + err.Error()
	}
	if ctx.Err() != nil {
		st.exit = -2
		st.out += "\nHUNG: killed after 15 minutes"
	}
	st.decs = s.newDecisions()
	return st
}

func (s *synth) newDecisions() []reuse.Decision {
	b, _ := os.ReadFile(s.log)
	if len(b) < s.logRead {
		s.logRead = 0
	}
	fresh := b[s.logRead:]
	s.logRead = len(b)
	var out []reuse.Decision
	sc := bufio.NewScanner(bytes.NewReader(fresh))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var d reuse.Decision
		if json.Unmarshal(sc.Bytes(), &d) == nil {
			out = append(out, d)
		}
	}
	return out
}

// token is one step's outcome for one package, in the notes' vocabulary.
func (st step) token(pkg string) (tok, how string) {
	var d *reuse.Decision
	for i := range st.decs {
		if st.decs[i].Package == pkg {
			d = &st.decs[i]
		}
	}
	switch {
	case st.exit == -2:
		return "HUNG", ""
	case d == nil && st.exit != 0:
		return "RED", firstLine(st.out)
	case d == nil:
		return "NONE", "no decision line for " + pkg
	case d.Audit == "mismatch":
		return "AUDIT-MISMATCH", d.Miss
	case d.Audit == "agree":
		return "AUDIT-AGREE", ""
	case d.Decision == "reuse":
		return "REUSE", ""
	case d.Signal != "":
		return "KILLED", d.Signal
	case d.Exit != 0:
		return "FAIL", d.Miss
	case !d.Stored:
		return "REFUSE", d.Refused
	}
	return "RUN", d.Miss
}

func (st step) decision(pkg string) *reuse.Decision {
	for i := range st.decs {
		if st.decs[i].Package == pkg {
			return &st.decs[i]
		}
	}
	return nil
}

// seq joins the steps' tokens for pkg, and the last step's reason as the how.
func seq(pkg string, steps ...step) (got, how string) {
	toks := make([]string, len(steps))
	var hows []string
	for i, st := range steps {
		tok, why := st.token(pkg)
		toks[i] = tok
		if why != "" && why != firstRunMiss {
			hows = append(hows, fmt.Sprintf("%d: %s", i+1, clip(why)))
		}
	}
	return strings.Join(toks, ","), strings.Join(hows, "; ")
}

// firstRunMiss is the miss every first run of a key has: not worth a line.
const firstRunMiss = "no entry for this binary, argv, cwd, env, identity and wrapper"

func clip(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 160 {
		return s[:160] + "..."
	}
	return s
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			return l
		}
	}
	return ""
}

// edit writes a file of the synth (relative to its repo root).
func (s *synth) edit(p, content string) {
	writeFile(s.h.t, filepath.Join(s.dir, filepath.FromSlash(p)), content)
}

func (s *synth) path(p string) string { return filepath.Join(s.dir, filepath.FromSlash(p)) }

func writeFile(t *testing.T, p, content string) {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// realCopy is a throwaway copy of the tree under test: every file git lists
// (tracked and untracked, not ignored), committed into a repo of its own.
func (h *harness) realCopy() (string, error) {
	if h.real != "" || h.realErr != nil {
		return h.real, h.realErr
	}
	dst := filepath.Join(h.work, "real")
	list := gitOut(h.t, h.repo, "ls-files", "-z", "-co", "--exclude-standard")
	for _, rel := range strings.Split(strings.TrimSuffix(list, "\x00"), "\x00") {
		src := filepath.Join(h.repo, filepath.FromSlash(rel))
		fi, err := os.Lstat(src)
		if err != nil || !fi.Mode().IsRegular() {
			continue // deleted on disk, or a link: not needed by the real probes
		}
		b, err := os.ReadFile(src)
		if err != nil {
			h.realErr = err
			return "", err
		}
		p := filepath.Join(dst, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			h.realErr = err
			return "", err
		}
		if err := os.WriteFile(p, b, fi.Mode().Perm()); err != nil {
			h.realErr = err
			return "", err
		}
	}
	gitOut(h.t, dst, "init", "-q")
	gitOut(h.t, dst, "add", "-A")
	gitOut(h.t, dst, "-c", "user.email=probe@example.invalid", "-c", "user.name=probe", "commit", "-qm", "probe copy")
	settle()
	h.real = dst
	return dst, nil
}

// realSynth is a synth over the real copy: one module dir of it, own store.
func (h *harness) realSynth(n int, module string) (*synth, error) {
	root, err := h.realCopy()
	if err != nil {
		return nil, err
	}
	return &synth{h: h, dir: root, mod: filepath.Join(root, module), store: filepath.Join(h.work, "stores", fmt.Sprint(n)),
		log: filepath.Join(h.work, "stores", fmt.Sprintf("%d.log", n)), identity: h.identity, mode: reuse.ModeReuse,
		wrapper: h.wrapper}, nil
}

// server is a TCP peer a probe's test talks to: the test sends one line and
// waits for the reply, so the probe can act while the binary is mid-run.
type server struct {
	ln    net.Listener
	reply func(line string) string
}

func newServer(t *testing.T) *server {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &server{ln: ln, reply: func(string) string { return "go" }}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				line, err := bufio.NewReader(c).ReadString('\n')
				if err != nil {
					return
				}
				if r := s.reply(strings.TrimSpace(line)); r != "" {
					fmt.Fprintln(c, r)
				}
			}(c)
		}
	}()
	return s
}

func (s *server) addr() string { return s.ln.Addr().String() }

// pkgTokens is "pkg=TOKENS" for each package of the module, sorted, and each
// package's reasons.
func pkgTokens(names []string, steps ...step) (got, how string) {
	names = append([]string{}, names...)
	sort.Strings(names)
	var parts, hows []string
	for _, n := range names {
		g, hw := seq("probe/"+n, steps...)
		parts = append(parts, n+"="+g)
		if hw != "" {
			hows = append(hows, n+" "+hw)
		}
	}
	return strings.Join(parts, " "), strings.Join(hows, "; ")
}
