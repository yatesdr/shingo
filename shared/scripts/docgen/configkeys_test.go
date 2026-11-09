package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cfgkeysFixture writes a config package and its snapshot under a temp root
// and returns the root.
func cfgkeysFixture(t *testing.T, src, snapshot string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "mod", "config")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "defaults.snapshot.txt"), []byte(snapshot), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

const cfgkeysFixtureSrc = `package config

import "time"

type Config struct {
	mu   int    ` + "`yaml:\"-\"`" + `
	Name string ` + "`yaml:\"name\"`" + ` // the station's name
	Hidden string ` + "`yaml:\"-\"`" + `
	Web  WebConfig ` + "`yaml:\"web\"`" + `
	Jobs []Job ` + "`yaml:\"jobs\"`" + `
}

type WebConfig struct {
	// The listen pair. Both fields share this comment.
	Host string ` + "`yaml:\"host\"`" + `
	Port int    ` + "`yaml:\"port\"`" + ` // 0 = pick one

	Secret string ` + "`yaml:\"secret\"`" + `
	Every  time.Duration ` + "`yaml:\"every\"`" + ` // a | pipe and a <tag>
}

type Job struct {
	At time.Duration ` + "`yaml:\"at\"`" + ` // when
}
`

const cfgkeysFixtureSnap = `# header
[FIELDS]
jobs = <empty>
name =
web.every = 5s
web.host = localhost
web.port = 8080
web.secret = <unset>

[RESOLVED]
web.effective = 1
`

// The walk's rules, each pinned on a row that only exists if the rule fired:
// a group comment speaks for the run it heads and stops at a blank line, a
// list of structs gets element rows, yaml:"-" and unexported fields are
// skipped, and a cell cannot break the table or open an HTML tag.
func TestCfgkeys_RowsFromFixture(t *testing.T) {
	root := cfgkeysFixture(t, cfgkeysFixtureSrc, cfgkeysFixtureSnap)
	got, err := cfgkeysEmit(root, "mod/config")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"| `jobs` | list | _(empty)_ |  |",
		"| `jobs[].at` | duration | — | when |",
		"| `name` | string | _(empty)_ | the station's name |",
		"| `web.every` | duration | `5s` | a \\| pipe and a &lt;tag> |",
		"| `web.host` | string | `localhost` | The listen pair. Both fields share this comment. |",
		"| `web.port` | int | `8080` | The listen pair. Both fields share this comment. (0 = pick one) |",
		"| `web.secret` | string | _(unset)_ |  |",
	}
	for _, w := range want {
		if !strings.Contains(got, w+"\n") {
			t.Errorf("missing row %s\n--- got:\n%s", w, got)
		}
	}
	for _, never := range []string{"hidden", "`mu`", "effective"} {
		if strings.Contains(got, never) {
			t.Errorf("%q reached the table:\n%s", never, got)
		}
	}
	// Element rows sit straight after their list.
	if strings.Index(got, "`jobs[].at`") < strings.Index(got, "`jobs`") ||
		strings.Index(got, "`jobs[].at`") > strings.Index(got, "`name`") {
		t.Errorf("jobs[].at is not straight after jobs:\n%s", got)
	}
}

// A snapshot and a struct that name different keys is a stale snapshot, and
// it is an error naming both sides, never a quietly partial table.
func TestCfgkeys_RefusesASnapshotThatDisagrees(t *testing.T) {
	stale := strings.Replace(cfgkeysFixtureSnap, "web.port = 8080\n", "web.old_port = 8080\n", 1)
	root := cfgkeysFixture(t, cfgkeysFixtureSrc, stale)
	_, err := cfgkeysEmit(root, "mod/config")
	if err == nil {
		t.Fatal("a snapshot missing web.port and carrying web.old_port was accepted")
	}
	for _, want := range []string{"web.port", "web.old_port", "make defaults-snapshot"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %q: %v", want, err)
		}
	}
}

// On the real tree: both modules' tables carry their headline keys with the
// shipped default, and no secret's value.
func TestCfgkeys_RealTree(t *testing.T) {
	cases := map[string][]string{
		"shingo-core/config": {
			"| `web.port` | int | `8083` |",
			"| `database.postgres.password` | string | _(unset)_ |",
			"| `web.session_secret` | string | _(set; redacted)_ |",
			"| `dispatch.futility.threshold` | int | `20` |",
		},
		"shingo-edge/config": {
			"| `web.port` | int | `8081` |",
			"| `backup.keep_hourly` | int | `48` |",
			"| `sim.processes[].tag_name` | string | — |",
		},
	}
	for dir, rows := range cases {
		got, err := cfgkeysEmit(repoRoot, dir)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		for _, r := range rows {
			if !strings.Contains(got, r) {
				t.Errorf("%s: no row starting %s", dir, r)
			}
		}
		if strings.Contains(got, "change-me") {
			t.Errorf("%s: a secret's value reached the table", dir)
		}
	}
}
