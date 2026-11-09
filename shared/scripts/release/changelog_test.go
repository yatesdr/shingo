package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseDeploy(t *testing.T) {
	cases := []struct {
		line    string
		named   map[string][]int
		none    bool
		wantErr string
	}{
		{"Deploy: no migration.", map[string][]int{}, true, ""},
		{"Deploy: Edge only; no migrations. Not deployed.", map[string][]int{}, true, ""},
		{"Deploy: Core v176; Edge v45–v46. Core first.", map[string][]int{"Core": {176}, "Edge": {45, 46}}, false, ""},
		{"Deploy: Core v8-v10, v12 and Edge v2..v3", map[string][]int{"Core": {8, 9, 10, 12}, "Edge": {2, 3}}, false, ""},
		{"Deploy: Core migrations v8–10 (each applies at boot)", map[string][]int{"Core": {8, 9, 10}}, false, ""},
		{"Deploy: Edge v44 — the same as v0.1.0's Core", map[string][]int{"Edge": {44}}, false, ""},
		{"Deploy: Core's v176 first, then the Edge's v45", map[string][]int{"Core": {176}, "Edge": {45}}, false, ""},
		{"Deploy: v176 first", nil, false, "before saying whose"},
		{"Deploy: Core v10–v8", nil, false, "does not read as low–high"},
	}
	for _, c := range cases {
		d, err := ParseDeploy(c.line)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%q: err %v, want one saying %q", c.line, err, c.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.line, err)
			continue
		}
		if !reflect.DeepEqual(d.Named, c.named) || d.None != c.none {
			t.Errorf("%q: named %v none %v, want %v %v", c.line, d.Named, d.None, c.named, c.none)
		}
	}
}

func TestDeployParagraph(t *testing.T) {
	body := []string{"Deploy: Core v176,", "Edge v45; Core first.", "", "- a change"}
	if got, want := deployParagraph(body), "Deploy: Core v176, Edge v45; Core first."; got != want {
		t.Errorf("paragraph %q, want %q", got, want)
	}
	if got := deployParagraph([]string{"- a change", "Deploy: no migration"}); got != "" {
		t.Errorf("a body that opens with a bullet has no Deploy paragraph, got %q", got)
	}
	if got := deployParagraph([]string{"Deploy: no migration", "- a change"}); got != "Deploy: no migration" {
		t.Errorf("a bullet ends the paragraph, got %q", got)
	}
}

func TestCompareDeploy(t *testing.T) {
	added := map[string][]int{"Core": {176, 177}, "Edge": {45}}
	cases := []struct {
		line string
		want []string // a substring per problem, in order
	}{
		{"Deploy: Core v176–v177, Edge v45", nil},
		{"Deploy: Core v176; Edge v45", []string{"does not name Core v177"}},
		{"Deploy: Core v176–v178; Edge v45", []string{"names Core v178, which no migration"}},
		{"Deploy: no migration", []string{"does not name Core v176–v177", "does not name Edge v45"}},
	}
	for _, c := range cases {
		d, err := ParseDeploy(c.line)
		if err != nil {
			t.Fatal(err)
		}
		got := compareDeploy(d, added, "v0.1.0")
		if len(got) != len(c.want) {
			t.Errorf("%q: problems %q, want %d", c.line, got, len(c.want))
			continue
		}
		for i := range got {
			if !strings.Contains(got[i], c.want[i]) {
				t.Errorf("%q: problem %q, want it to say %q", c.line, got[i], c.want[i])
			}
		}
	}

	none := map[string][]int{}
	for line, want := range map[string]string{
		"Deploy: no migration; Core only": "",
		"Deploy: Core only":               "does not say `no migration`",
		"Deploy: Core v176":               "names Core v176, and no migration was added",
	} {
		d, err := ParseDeploy(line)
		if err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		got := compareDeploy(d, none, "v0.1.0")
		switch {
		case want == "" && len(got) > 0:
			t.Errorf("%q: problems %q, want none", line, got)
		case want != "" && (len(got) != 1 || !strings.Contains(got[0], want)):
			t.Errorf("%q: problems %q, want one saying %q", line, got, want)
		}
	}
}

const changelogText = "# Changelog\n\nIntro.\n\n## Unreleased\n\nDeploy: no migration.\n\n- A change\n\n" +
	"## v0.2.0 — 2026-02-01 — Two\n\nDeploy: Core v9.\n\n- Two's change\n\n" +
	"```\n## v0.1.5 inside a fence is not a heading\n```\n\n" +
	"## Before v0.1.0\n\n### 2026-01-01 — Old\n\n- An old change\n"

func TestNotes(t *testing.T) {
	got, err := Notes(changelogText, "v0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	want := "Deploy: Core v9.\n\n- Two's change\n\n```\n## v0.1.5 inside a fence is not a heading\n```\n"
	if got != want {
		t.Errorf("notes:\n%s\nwant:\n%s", got, want)
	}
	for _, v := range []string{"v0.1.5", "v0.1.0", "v0.2"} {
		if _, err := Notes(changelogText, v); err == nil {
			t.Errorf("Notes(%s) found a section; there is none", v)
		}
	}
	if _, err := Notes("## v0.3.0 — 2026-03-01\n\n", "v0.3.0"); err == nil {
		t.Error("an empty section gave notes")
	}
}

func TestCut(t *testing.T) {
	got, err := Cut(changelogText, ReleaseHeading("v0.3.0", "2026-03-01", "Three"))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(changelogText, "## Unreleased\n", "## Unreleased\n\n## v0.3.0 — 2026-03-01 — Three\n", 1)
	if got != want {
		t.Errorf("cut:\n%s\nwant:\n%s", got, want)
	}
	if n, err := Notes(got, "v0.3.0"); err != nil || n != "Deploy: no migration.\n\n- A change\n" {
		t.Errorf("the cut section's notes are %q", n)
	}
	if _, err := Cut("# Changelog\n", "v0.3.0 — 2026-03-01"); err == nil {
		t.Error("Cut without an Unreleased section succeeded")
	}
	if _, err := Cut("## Unreleased\n\n## Unreleased\n", "v0.3.0 — 2026-03-01"); err == nil {
		t.Error("Cut with two Unreleased sections succeeded")
	}
	above := "## 2026-11-09 — A merge-log entry\n\n- A change\n\n" + changelogText[strings.Index(changelogText, "## Unreleased"):]
	if _, err := Cut(above, "v0.3.0 — 2026-03-01"); err == nil || !strings.Contains(err.Error(), "is above `## Unreleased`") {
		t.Errorf("Cut with a section above Unreleased: %v, want it refused", err)
	}
	if h := ReleaseHeading("v0.3.0", "2026-03-01", ""); h != "v0.3.0 — 2026-03-01" {
		t.Errorf("an untitled heading is %q", h)
	}
}
