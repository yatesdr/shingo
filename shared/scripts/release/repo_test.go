package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseVersion(t *testing.T) {
	for _, s := range []string{"v0.1.0", "v0.0.14", "v1.20.3", "v10.0.0"} {
		v, ok := ParseVersion(s)
		if !ok || v.String() != s {
			t.Errorf("ParseVersion(%q) = %v, %v", s, v, ok)
		}
	}
	for _, s := range []string{"", "0.1.0", "v0.1", "v0.1.0.1", "v0.01.0", "v0.1.0-rc1", "V0.1.0", " v0.1.0"} {
		if _, ok := ParseVersion(s); ok {
			t.Errorf("ParseVersion(%q) accepted a malformed version", s)
		}
	}
	a, _ := ParseVersion("v0.9.3")
	b, _ := ParseVersion("v0.10.0")
	if !a.Less(b) || b.Less(a) {
		t.Error("v0.9.3 is below v0.10.0: versions order by number, not text")
	}
	if got := a.Next(Minor).String(); got != "v0.10.0" {
		t.Errorf("v0.9.3 MINOR = %s", got)
	}
	if got := a.Next(Patch).String(); got != "v0.9.4" {
		t.Errorf("v0.9.3 PATCH = %s", got)
	}
}

func TestIsSource(t *testing.T) {
	cases := map[string]bool{
		"shingo-core/engine/dispatch.go":           true,
		"shingo-edge/store/schema/snapshot.sql":    true,
		"protocol/go.mod":                          true,
		"shingo-core/www/templates/bins.html":      true,
		"shingo-edge/www/static/js/board.js":       true,
		"shared/tokens.css":                        true,
		"shared/icons.svg":                         true,
		"shingo-core/www/static/favicon.ico":       true,
		"integration/harness/bus.go":               true,
		"shingo-core/engine/dispatch_test.go":      false,
		"shared/utils.escape.test.js":              false,
		"shingo-edge/www/testdata/page.html":       false,
		"shingo-core/README.md":                    false,
		"shingo-core/go.sum":                       false,
		"docs/ground-rules.md":                     false,
		"scripts/gate.sh":                          false,
		"plants/master.yaml":                       false,
		"CHANGELOG.md":                             false,
		"shingo-core-old/x.go":                     false,
		"shingo-edge/config/defaults.snapshot.txt": false,
	}
	for p, want := range cases {
		if got := IsSource(p); got != want {
			t.Errorf("IsSource(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestMigrationVersions(t *testing.T) {
	cases := []struct {
		name, src string
		want      []int
		wantErr   string
	}{
		{"core", `package store
func migrationList() []migration {
	return []migration{
		{1, "one", v1, nil},
		{2, "two", v2, func(q Querier) bool { return true }},
	}
}
func other() { _ = []struct{ table, col string }{{"orders", "x"}}; _ = [][2]int{{3, 4}} }`, []int{1, 2}, ""},
		{"core at v0.0.14", `package store
func (db *DB) run() error {
	migrations := []struct {
		version int
		name    string
		fn      func() error
	}{
		{1, "one", db.v1},
		{7, "seven", db.v7},
	}
	_ = migrations
	return nil
}`, []int{1, 7}, ""},
		{"edge", `package store
func edgeMigrations() []migrate.Migration {
	return []migrate.Migration{
		{Version: 3, Name: "three", Fn: f},
		{
			Version: 2,
			Name:    "two",
		},
		{Version: 9},
	}
}`, []int{2, 3}, ""},
		{"none", "package store\n", nil, ""},
		{"twice", `package store
var ms = []migration{{4, "a", nil, nil}, {4, "b", nil, nil}}`, nil, "v4 more than once"},
	}
	for _, c := range cases {
		got, err := MigrationVersions(c.name+".go", []byte(c.src))
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%s: err %v, want one saying %q", c.name, err, c.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: versions %v, want %v", c.name, got, c.want)
		}
	}
}

func TestConfigKeys(t *testing.T) {
	src := "package config\n" +
		"type Config struct {\n" +
		"	mu     sync.Mutex `yaml:\"-\"`\n" +
		"	Web    WebConfig `yaml:\"web\"`\n" +
		"	PLCs   []PLC `yaml:\"plcs,omitempty\"`\n" +
		"	Labels map[string]*Label `yaml:\"labels\"`\n" +
		"	Base   `yaml:\",inline\"`\n" +
		"	Hidden string `yaml:\"-\"`\n" +
		"	Timeout time.Duration\n" +
		"	self   *Config\n" +
		"}\n" +
		"type WebConfig struct{ Port int `yaml:\"port\"`; Next *Config `yaml:\"next\"` }\n" +
		"type PLC struct{ Name string `yaml:\"name\"` }\n" +
		"type Label struct{ Color string `yaml:\"color\"` }\n" +
		"type Base struct{ Zone string `yaml:\"timezone\"` }\n" +
		"type Unreached struct{ X int `yaml:\"x\"` }\n"
	got, err := ConfigKeys(map[string][]byte{"config.go": []byte(src)})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"labels", "labels.*.color", "plcs", "plcs[].name", "timeout", "timezone", "web", "web.next", "web.port"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("keys %v,\nwant %v", got, want)
	}
	if _, err := ConfigKeys(map[string][]byte{"x.go": []byte("package config\ntype Settings struct{}\n")}); err == nil {
		t.Error("a package with no Config type gave keys")
	}
}

func TestFormatVersions(t *testing.T) {
	cases := map[string][]int{
		"v3":            {3},
		"v3, v5–v6":     {6, 3, 5},
		"v1–v3, v7":     {1, 2, 3, 3, 7},
		"v2–v4, v9–v10": {2, 3, 4, 9, 10},
		"":              nil,
	}
	for want, vs := range cases {
		if got := FormatVersions(vs); got != want {
			t.Errorf("FormatVersions(%v) = %q, want %q", vs, got, want)
		}
	}
}
