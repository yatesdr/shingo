package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The CMS block's contract in four properties: empty base_url disables it,
// a base_url without credentials refuses to boot, zero durations fill in from
// the defaults, and the keys never reach a rendered string.

func TestCMSConfig_Validate_EmptyURLDisabled(t *testing.T) {
	t.Parallel()
	c := CMSDefaults()
	if c.Enabled() {
		t.Error("the shipped default is enabled — a site with no cms: block must post nothing")
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate on a disabled block returned %v; a site not using CMS is not misconfigured", err)
	}
}

func TestCMSConfig_Validate_MissingKeysIsAnError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name            string
		access, secret  string
		wantErr         bool
		wantErrMentions string
	}{
		{name: "both present", access: "AK", secret: "SK", wantErr: false},
		{name: "no access key", access: "", secret: "SK", wantErr: true, wantErrMentions: "access_key"},
		{name: "no secret key", access: "AK", secret: "", wantErr: true, wantErrMentions: "secret_key"},
		{name: "neither", access: "", secret: "", wantErr: true, wantErrMentions: "access_key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := CMSDefaults()
			c.BaseURL = "https://middleware.example.invalid/api"
			c.AccessKey, c.SecretKey = tc.access, tc.secret
			err := c.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("Validate accepted base_url with access=%q secret=%q — the integration "+
					"would accept every movement and post none", tc.access, tc.secret)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate rejected a complete config: %v", err)
			}
			if tc.wantErr && !strings.Contains(err.Error(), tc.wantErrMentions) {
				t.Errorf("error %q does not name %s — a refusal has to say which key is missing",
					err, tc.wantErrMentions)
			}
			// A refusal must not print the keys it is complaining about.
			if tc.wantErr && strings.Contains(err.Error(), "AK") {
				t.Errorf("the error carries the access key: %v", err)
			}
		})
	}
}

func TestCMSConfig_Validate_FillsZeroDurations(t *testing.T) {
	t.Parallel()
	// A hand-written block that omits the timings is the common case, and every
	// one of these at zero is a different silent failure: a zero timeout hangs
	// the poster, a zero poll interval spins it, zero attempts never retries,
	// and a zero settle window reconciles a POST before the middleware can
	// have indexed it.
	c := CMSConfig{BaseURL: "https://x.invalid", AccessKey: "AK", SecretKey: "SK"}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if c.Timeout <= 0 || c.PollInterval <= 0 || c.MaxAttempts <= 0 || c.SettleWindow <= 0 {
		t.Errorf("zero durations survived Validate: %+v", c)
	}
}

// TestCMSConfig_KeysRedactedInEveryRendering is the leak guard. The keys must
// not survive a %v, a %s, or a %+v of the enclosing Config — the three ways a
// debug print, a panic dump, or a log line written in a hurry gets at them.
func TestCMSConfig_KeysRedactedInEveryRendering(t *testing.T) {
	t.Parallel()
	const accessKey = "AKIA-RECOGNISABLE-ACCESS-000"
	const secretKey = "SECRET-RECOGNISABLE-VALUE-999"

	cfg := Defaults()
	cfg.CMS.BaseURL = "https://middleware.example.invalid/api"
	cfg.CMS.AccessKey = accessKey
	cfg.CMS.SecretKey = secretKey

	// The verbs go through a func(any) so the value reaches fmt as an
	// interface. Handing cfg.CMS to fmt.Sprintf("%s", ...) directly is a
	// staticcheck S1025 ("should use String()") — correct advice in general,
	// and exactly the call this test has to make, because %s on a Stringer is
	// one of the ways a key escapes.
	render := func(verb string, v any) string { return fmt.Sprintf(verb, v) }
	for _, rendered := range []string{
		render("%v", cfg.CMS),
		render("%s", cfg.CMS),
		render("%+v", cfg.CMS),
		render("%v", &cfg.CMS),
		cfg.CMS.String(),
	} {
		if strings.Contains(rendered, accessKey) || strings.Contains(rendered, secretKey) {
			t.Errorf("a key survived a rendering: %s", rendered)
		}
		if !strings.Contains(rendered, "<set>") {
			t.Errorf("rendering does not say the keys are present: %s", rendered)
		}
	}

	// And the other direction: an unset key reads as unset, not as a redacted
	// one. "<set>" for an empty key would hide a misconfiguration.
	empty := CMSDefaults()
	if !strings.Contains(empty.String(), "<empty>") {
		t.Errorf("an unconfigured block does not report its keys as empty: %s", empty.String())
	}
}

// TestConfigLoad_CMSValidateIsFatal is the wiring pin. Validate being correct
// is worth nothing if Load does not call it, and Load's convention for the
// neighbouring RDS block is to repair and continue — so a reader could
// reasonably assume the same here.
func TestConfigLoad_CMSValidateIsFatal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	broken := filepath.Join(dir, "broken.yaml")
	mustWrite(t, broken, "cms:\n  base_url: https://middleware.example.invalid/api\n")
	if _, err := Load(broken); err == nil {
		t.Error("Load accepted a cms: block with a base_url and no keys — it must refuse to boot")
	}

	whole := filepath.Join(dir, "whole.yaml")
	mustWrite(t, whole, "cms:\n  base_url: https://middleware.example.invalid/api\n"+
		"  access_key: AK\n  secret_key: SK\n")
	cfg, err := Load(whole)
	if err != nil {
		t.Fatalf("Load rejected a complete cms: block: %v", err)
	}
	if !cfg.CMS.Enabled() {
		t.Error("a configured block did not come back enabled")
	}
	if cfg.CMS.SettleWindow != 5*time.Minute {
		t.Errorf("settle_window = %s, want the 5m default filled in", cfg.CMS.SettleWindow)
	}

	absent := filepath.Join(dir, "absent.yaml")
	mustWrite(t, absent, "web:\n  port: 8080\n")
	cfg, err = Load(absent)
	if err != nil {
		t.Fatalf("Load refused a config with no cms: block: %v", err)
	}
	if cfg.CMS.Enabled() {
		t.Error("a config with no cms: block came back enabled")
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
