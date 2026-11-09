package main

import (
	"strings"
	"testing"
)

func TestDriftTests_ListsEveryDriftTest(t *testing.T) {
	root := gosrcWriteTree(t, map[string]string{
		"go.work":  "go 1.25\n\nuse ./m\n",
		"m/go.mod": "module m\n",
		"m/www/a_drift_test.go": `package www

import "testing"

// TestAPinsB holds A to B.
func TestAPinsB(t *testing.T) {}

func TestUndocumented(t *testing.T) {}

func TestMain(m *testing.M) {}

func Testlowercase(t *testing.T) {}

func helper(t *testing.T) {}
`,
		"m/www/plain_test.go":                "package www\n\nimport \"testing\"\n\nfunc TestNotDrift(t *testing.T) {}\n",
		"m/testdata/x/fixture_drift_test.go": "package x\n\nimport \"testing\"\n\nfunc TestFixture(t *testing.T) {}\n",
	})
	got, err := drifttestsTable(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{
		"| `m/www/a_drift_test.go` | `TestAPinsB` | TestAPinsB holds A to B. |",
		"| `m/www/a_drift_test.go` | `TestUndocumented` | — |",
	} {
		if !strings.Contains(got, w+"\n") {
			t.Errorf("no row %s in\n%s", w, got)
		}
	}
	for _, n := range []string{"TestMain", "Testlowercase", "helper", "TestNotDrift", "TestFixture"} {
		if strings.Contains(got, n) {
			t.Errorf("inventory lists %s:\n%s", n, got)
		}
	}
}

func TestDriftTests_TheRealInventory(t *testing.T) {
	got, err := drifttestsTable(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"`TestTransitionsDocTableMatchesTheCode`", "`TestNoUndefinedCSSVarsInTemplates`", "`TestSwapModeWordsMatchTheModel`"} {
		if !strings.Contains(got, w) {
			t.Errorf("inventory has no %s", w)
		}
	}
	if strings.Contains(got, "TestShapeFieldWordsMatchTheModel") {
		t.Error("inventory lists the deleted TestShapeFieldWordsMatchTheModel")
	}
}
