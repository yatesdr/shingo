package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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

	// The whole Config, not just the CMS block. The stated risk was that a
	// `%+v` of a *Config spills the keys — CMSConfig.String() is what stops
	// that, and nothing above actually renders the container that would carry
	// it.
	for _, rendered := range []string{
		render("%v", cfg),
		render("%+v", cfg),
		render("%v", &cfg),
		render("%+v", &cfg),
	} {
		if strings.Contains(rendered, accessKey) || strings.Contains(rendered, secretKey) {
			t.Errorf("a key survived a rendering of the whole Config: %s", rendered)
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

// TestCMSConfig_Validate_RejectsAnUnusableBaseURL.
//
// Validation is FATAL for this block, and this is the case that most needs it:
// a base_url with a typo boots fine, and the failure then happens inside every
// POST, one row at a time, in a table nobody is watching. Before the fix that
// arrived as ClassRejected — terminal — so a single wrong character marked
// every posting permanently refused.
//
// The scheme and host are checked, not merely "url.Parse returned no error",
// because url.Parse accepts almost anything: a host with the https:// left off
// parses cleanly as a relative path.
func TestCMSConfig_Validate_RejectsAnUnusableBaseURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		url  string
	}{
		{"scheme forgotten", "middleware.example.com/api"},
		{"host missing", "https://"},
		{"not a url at all", "not a url"},
		{"control character", "http://exam\x7fple.invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := CMSDefaults()
			c.BaseURL = tc.url
			c.AccessKey, c.SecretKey = "AK", "SK"
			if err := c.Validate(); err == nil {
				t.Errorf("base_url %q was accepted. Core would boot, post nothing, and "+
					"record the reason in a table nobody reads.", tc.url)
			}
		})
	}
}

// TestCMSConfig_Validate_AcceptsAUsableBaseURL is the selectivity half: a
// validator that refuses everything is not a validator.
func TestCMSConfig_Validate_AcceptsAUsableBaseURL(t *testing.T) {
	t.Parallel()
	for _, u := range []string{
		"https://middleware.example.com/api/inventory_transactions",
		"http://10.0.0.5:8080/inventory",
		"https://middleware.example.com/api?tenant=acme",
	} {
		c := CMSDefaults()
		c.BaseURL = u
		c.AccessKey, c.SecretKey = "AK", "SK"
		if err := c.Validate(); err != nil {
			t.Errorf("base_url %q was refused: %v", u, err)
		}
	}
}

// TestCMSConfig_Validate_EmptyBaseURLIsNotAnError: an empty base_url is how a
// site says it does not use CMS, and the rest of the block is then irrelevant —
// including the keys.
func TestCMSConfig_Validate_EmptyBaseURLIsNotAnError(t *testing.T) {
	t.Parallel()
	c := CMSDefaults()
	if err := c.Validate(); err != nil {
		t.Errorf("an unconfigured cms: block was refused: %v", err)
	}
}

// TestCMSConfig_MappersCarryEveryField walks the three destination structs by
// REFLECTION rather than naming their fields.
//
// The mapping it guards replaced twelve lines of hand copying, and hand copying
// is the shape where a field added to CMSConfig is silently not carried: adding
// one breaks no build, the subsystem simply runs on a zero value nobody chose.
// Naming the fields here would inherit exactly that problem — the test would
// pass for the same reason the code was wrong. Walking them means a new field
// with no mapping is a failure the day it is added.
//
// Every source field is given a non-zero value first, so "carried" and "left at
// its default" are distinguishable.
func TestCMSConfig_MappersCarryEveryField(t *testing.T) {
	t.Parallel()
	c := CMSConfig{
		BaseURL: "https://mw.example.com/api", AccessKey: "AK", SecretKey: "SK",
		Timeout: 7 * time.Second, PollInterval: 11 * time.Second, MaxAttempts: 5,
		SettleWindow: 13 * time.Minute, MaxRequeues: 9, HealthWindow: 17 * time.Hour,
		ReasonCode: "RC", IncreaseType: "IT", DecreaseType: "DT",
		UnitOfMeasure: "UOM", UserID: "UID",
		Department: "DEPT", Operation: "OP",
	}

	for _, tc := range []struct {
		name string
		got  any
		// skip names destination fields this block legitimately does not fill.
		skip map[string]string
	}{
		{name: "client.Config", got: c.Client()},
		{name: "wire.Config", got: c.Wire()},
		{
			name: "poster.Config",
			got:  c.Poster(),
			// Wire is a nested struct, checked by its own row above.
			skip: map[string]string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := reflect.ValueOf(tc.got)
			for i := 0; i < v.NumField(); i++ {
				f := v.Type().Field(i)
				if why, ok := tc.skip[f.Name]; ok {
					t.Logf("%s.%s skipped: %s", tc.name, f.Name, why)
					continue
				}
				if v.Field(i).IsZero() {
					t.Errorf("%s.%s is the zero value. Either the mapper does not carry it — "+
						"in which case the subsystem runs on a default nobody chose — or this "+
						"test's source config does not set it.", tc.name, f.Name)
				}
			}
		})
	}
}

// TestCMSConfig_MappersMatchTheSourceValues is the other half: carrying SOME
// non-zero value is not the same as carrying the right one. A mapper that
// swapped two same-typed fields — PollInterval and SettleWindow, say — passes
// the reflection walk above and is badly wrong.
func TestCMSConfig_MappersMatchTheSourceValues(t *testing.T) {
	t.Parallel()
	c := CMSDefaults()
	c.BaseURL, c.AccessKey, c.SecretKey = "https://mw.example.com/api", "AK", "SK"
	c.Timeout, c.PollInterval, c.SettleWindow = 7*time.Second, 11*time.Second, 13*time.Minute
	c.MaxAttempts, c.MaxRequeues = 5, 9
	c.Department, c.Operation = "DEPT", "OP"

	cl, p, w := c.Client(), c.Poster(), c.Wire()
	for _, tc := range []struct {
		name      string
		got, want any
	}{
		{"client.BaseURL", cl.BaseURL, c.BaseURL},
		{"client.AccessKey", cl.AccessKey, c.AccessKey},
		{"client.SecretKey", cl.SecretKey, c.SecretKey},
		{"client.Timeout", cl.Timeout, c.Timeout},
		{"poster.PollInterval", p.PollInterval, c.PollInterval},
		{"poster.MaxAttempts", p.MaxAttempts, c.MaxAttempts},
		{"poster.SettleWindow", p.SettleWindow, c.SettleWindow},
		{"poster.MaxRequeues", p.MaxRequeues, c.MaxRequeues},
		{"poster.Wire", p.Wire, w},
		{"wire.ReasonCode", w.ReasonCode, c.ReasonCode},
		{"wire.IncreaseType", w.IncreaseType, c.IncreaseType},
		{"wire.DecreaseType", w.DecreaseType, c.DecreaseType},
		{"wire.UnitOfMeasure", w.UnitOfMeasure, c.UnitOfMeasure},
		{"wire.UserID", w.UserID, c.UserID},
		{"wire.Department", w.Department, c.Department},
		{"wire.Operation", w.Operation, c.Operation},
	} {
		if !reflect.DeepEqual(tc.got, tc.want) {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

// TestCMSConfig_ValidateFillsEveryShippedDefault walks CMSDefaults' non-zero
// fields and asserts Validate arrives at the same values from a zero config.
//
// The two are separate code paths onto the same numbers: CMSDefaults is what a
// fresh config file starts from, Validate is what fills a field a site left out
// of its yaml. A field added to one and not the other means a site that omits
// it gets a value nobody chose — silently, because a zero duration is a legal
// value everywhere it lands.
//
// Reflection rather than a list of field names, for the same reason as the
// mapper test: a list would have to be edited by the same person who forgot to
// edit Validate.
func TestCMSConfig_ValidateFillsEveryShippedDefault(t *testing.T) {
	t.Parallel()
	// The minimum a site must supply; everything else is left zero.
	got := CMSConfig{BaseURL: "https://mw.example.com/api", AccessKey: "AK", SecretKey: "SK"}
	if err := got.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	want := CMSDefaults()
	wv, gv := reflect.ValueOf(want), reflect.ValueOf(got)
	for i := 0; i < wv.NumField(); i++ {
		f := wv.Type().Field(i)
		// Only the fields CMSDefaults actually ships a value for. BaseURL and
		// the keys are the site's to supply and are deliberately empty here.
		if wv.Field(i).IsZero() {
			continue
		}
		// DURATIONS AND COUNTS ONLY, and the exclusion is not laziness.
		// Load starts from Defaults() and unmarshals the yaml over it, so an
		// OMITTED key keeps its default either way; Validate's fallbacks are
		// for a key written explicitly as zero. For a duration or a count that
		// is never a sensible value — a zero timeout means wait forever — so
		// clamping is right. For the vocabulary strings it is a legitimate
		// choice: department and operation are empty BY DESIGN, and a Validate
		// that refilled them would make an intentionally blank field
		// unsettable.
		switch f.Type.Kind() {
		case reflect.Int64, reflect.Int:
		default:
			continue
		}
		if !reflect.DeepEqual(gv.Field(i).Interface(), wv.Field(i).Interface()) {
			t.Errorf("after Validate, %s = %v; CMSDefaults ships %v. A site that omits this "+
				"field from its yaml gets a value nobody chose.",
				f.Name, gv.Field(i).Interface(), wv.Field(i).Interface())
		}
	}
}
