package main

// The timing flag: while `gatetime run` times a command it holds a directory
// in the clone's git common dir, `<common>/gate.timing`, beside the gate lock
// and shared by every worktree of the clone. Every heavy mode of
// scripts/gate.sh refuses to start while it is held and names the holder,
// GATE_NO_LOCK=1 included (scripts/gate-timing.sh reads it; the format below
// is the contract between the two, pinned by scripts/check-gate-lock.sh).
// The timed run's own gate passes: gatetime hands the command the flag's
// token in GATE_TIMING_TOKEN.
//
//	<dir>/owner      key=value lines: holder, pid, cwd, out, cmd, started, token
//	<dir>/heartbeat  touched every sampler pass; a heartbeat older than
//	                 flagStale means the holder is gone (killed, or the machine
//	                 slept), and the next `gatetime run` takes the flag over
//
// GATE_TIMING_DIR names another directory (the pin's private one).

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"
)

const (
	flagEnvDir   = "GATE_TIMING_DIR"
	flagEnvToken = "GATE_TIMING_TOKEN"
	flagStale    = 120 * time.Second
)

type timingFlag struct{ dir, token string }

// flagDir is where the flag lives for the checkout at repo, or "" when repo is
// not in a git checkout. A gate mirror on the Linux side
// (scripts/gate-mirror.sh) records the Windows clone's flag as gate.timingdir
// in its config, so a run timed inside the mirror holds the flag every gate on
// both sides reads, never one of the mirror's own.
func flagDir(repo string) string {
	if d := os.Getenv(flagEnvDir); d != "" {
		return d
	}
	if out, err := exec.Command("git", "-C", repo, "config", "--get", "gate.timingdir").Output(); err == nil {
		if d := strings.TrimSpace(string(out)); d != "" {
			return d
		}
	}
	out, err := exec.Command("git", "-C", repo, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		return ""
	}
	return filepath.Join(strings.TrimSpace(string(out)), "gate.timing")
}

// readOwner is the owner file as key/value pairs.
func readOwner(dir string) map[string]string {
	kv := map[string]string{}
	data, err := os.ReadFile(filepath.Join(dir, "owner"))
	if err != nil {
		return kv
	}
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r", ""), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			kv[k] = v
		}
	}
	return kv
}

// heartbeatAge is how long ago the holder last touched its heartbeat (the
// directory's own mtime when it has none yet).
func heartbeatAge(dir string) (time.Duration, bool) {
	fi, err := os.Stat(filepath.Join(dir, "heartbeat"))
	if err != nil {
		if fi, err = os.Stat(dir); err != nil {
			return 0, false
		}
	}
	return time.Since(fi.ModTime()), true
}

// acquireFlag takes the flag for one timed run, or says who holds it.
func acquireFlag(dir, cwd, out, cmd string) (*timingFlag, error) {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, err
	}
	for try := 0; ; try++ {
		err := os.Mkdir(dir, 0o755)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) || try > 2 {
			return nil, err
		}
		age, ok := heartbeatAge(dir)
		if ok && age < flagStale {
			o := readOwner(dir)
			return nil, fmt.Errorf("a timed run already holds the timing flag %s: %s pid %s, %q, since %s (heartbeat %.0fs ago)",
				dir, o["holder"], o["pid"], o["cmd"], o["started"], age.Seconds())
		}
		// Abandoned: rename first, so only one taker wins, then remove.
		moved := fmt.Sprintf("%s.stale.%d", dir, os.Getpid())
		if os.Rename(dir, moved) == nil {
			fmt.Fprintf(os.Stderr, "gatetime: taking the timing flag from %s (heartbeat %.0fs old)\n", readOwner(moved)["holder"], age.Seconds())
			_ = os.RemoveAll(moved)
		}
	}
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	f := &timingFlag{dir: dir, token: fmt.Sprintf("%d:%s", os.Getpid(), hex.EncodeToString(b))}
	who := "unknown"
	if u, err := user.Current(); err == nil {
		who = u.Username
	}
	host, _ := os.Hostname()
	owner := fmt.Sprintf("holder=%s@%s\npid=%d\ncwd=%s\nout=%s\ncmd=%s\nstarted=%s\ntoken=%s\n",
		who, host, os.Getpid(), filepath.ToSlash(cwd), filepath.ToSlash(out), oneLine(cmd),
		time.Now().UTC().Format(time.RFC3339), f.token)
	if err := os.WriteFile(filepath.Join(dir, "owner"), []byte(owner), 0o644); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	f.beat()
	return f, nil
}

// beat touches the heartbeat; written last at acquire, so a reader that sees
// it sees the owner file too.
func (f *timingFlag) beat() {
	p := filepath.Join(f.dir, "heartbeat")
	now := time.Now()
	if err := os.Chtimes(p, now, now); err != nil {
		_ = os.WriteFile(p, nil, 0o644)
	}
}

// release removes the flag if it is still this run's.
func (f *timingFlag) release() {
	if f == nil {
		return
	}
	if readOwner(f.dir)["token"] == f.token {
		_ = os.RemoveAll(f.dir)
	}
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
