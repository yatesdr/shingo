package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestDefaultsSnapshotIsCurrent fails when a shipped default changed and the
// committed rendering did not. The diff is the point: retuning what every
// installation gets should be a line somebody can ask about.
func TestDefaultsSnapshotIsCurrent(t *testing.T) {
	// DefaultsSnapshotPath is module-root relative; the test runs in config/.
	path := filepath.Join("..", filepath.FromSlash(DefaultsSnapshotPath))
	committed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v\n\nrun `%s` and commit the result", DefaultsSnapshotPath, err, DefaultsRegenCommand)
	}
	want := strings.ReplaceAll(string(committed), "\r\n", "\n")
	got := RenderDefaults()
	if got == want {
		return
	}
	t.Fatalf("the shipped defaults have changed and the snapshot has not — run `%s` and commit it.\n\n%s",
		DefaultsRegenCommand, firstDefaultsDiff(want, got))
}

// Rendering twice and comparing catches random values, timestamps and
// map-iteration order at once; any of them would fail the staleness test
// forever on a tree nobody touched.
func TestRenderDefaults_Deterministic(t *testing.T) {
	if first, second := RenderDefaults(), RenderDefaults(); first != second {
		t.Fatalf("RenderDefaults is not deterministic:\n%s", firstDefaultsDiff(first, second))
	}
}

// The shipped session secret, the database password and the CMS and signing
// keys are secrets by name; none of their values may reach the file.
func TestRenderDefaults_RedactsSecrets(t *testing.T) {
	rendered := RenderDefaults()
	cfg := Defaults()
	if cfg.Web.SessionSecret != "" && strings.Contains(rendered, cfg.Web.SessionSecret) {
		t.Error("the shipped session secret was rendered into the snapshot — it would be committed to git")
	}
	for _, key := range []string{"web.session_secret", "database.postgres.password", "cms.access_key",
		"cms.secret_key", "messaging.signing_key", "notifications.smtp_password"} {
		line := lineFor(rendered, key)
		if line == "" {
			t.Errorf("%s is not in the rendering", key)
			continue
		}
		if _, value, _ := strings.Cut(line, " = "); !strings.HasPrefix(value, "<") {
			t.Errorf("%s rendered a value, not a redaction marker: %q", key, line)
		}
	}
}

// The output scan is the backstop behind the tag and the name heuristic.
func TestRenderDefaults_NoUnredactedSecrets(t *testing.T) {
	if bad := ScanForUnredactedSecrets(RenderDefaults()); len(bad) > 0 {
		t.Errorf("credential-shaped values reached the snapshot:\n  %s", strings.Join(bad, "\n  "))
	}
}

// And the scan has to be able to FAIL, or it is decoration.
func TestScanForUnredactedSecrets_CatchesALeak(t *testing.T) {
	leaked := "web.session_secret = d9c7c94ef0d19db0f5be0f88f44494d3456162d74abcafc507d38e2c5d8db10e"
	if bad := ScanForUnredactedSecrets(leaked); len(bad) == 0 {
		t.Error("the scan did not catch a rendered 64-char secret — it would pass anything")
	}
	ordinary := "rds.base_url = http://localhost:8088\nmessaging.orders_topic = shingo.orders\nrds.poll_interval = 5s"
	if bad := ScanForUnredactedSecrets(ordinary); len(bad) > 0 {
		t.Errorf("the scan flagged ordinary values, which would make it noise: %v", bad)
	}
}

func lineFor(rendered, key string) string {
	for line := range strings.SplitSeq(rendered, "\n") {
		if strings.HasPrefix(line, key+" = ") {
			return line
		}
	}
	return ""
}

func firstDefaultsDiff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(al) && i < len(bl); i++ {
		if al[i] != bl[i] {
			return "- committed: " + al[i] + "\n+ built:     " + bl[i]
		}
	}
	if len(al) != len(bl) {
		return "(the two renderings have different lengths: " +
			strconv.Itoa(len(al)) + " vs " + strconv.Itoa(len(bl)) + " lines)"
	}
	return "(identical)"
}
