package store

import (
	"testing"
	"time"

	"shingocore/config"
)

// TestPgxConnConfig_PinsUTCTimeZone pins that every connection sets the
// session TimeZone to UTC, so a zoneless timestamp literal can never be
// re-localized to the DB server's OS TZ (the R20-1 / PurgeOldOutbox class).
func TestPgxConnConfig_PinsUTCTimeZone(t *testing.T) {
	cc, err := pgxConnConfig(&config.PostgresConfig{
		Host:     "localhost",
		Port:     5432,
		Database: "shingo",
		User:     "u",
		Password: "p",
		SSLMode:  "disable",
	})
	if err != nil {
		t.Fatalf("pgxConnConfig: %v", err)
	}
	if got := cc.RuntimeParams["timezone"]; got != "UTC" {
		t.Errorf("RuntimeParams[timezone] = %q, want %q", got, "UTC")
	}
	// Sanity: the DSN was actually parsed (not silently empty).
	if cc.Host != "localhost" {
		t.Errorf("Host = %q, want localhost — DSN not parsed", cc.Host)
	}
}

// TestPgxConnConfig_TimeoutsSurviveDSNParsing is the ninety-second-gate half of
// session_timeouts_docker_test.go.
//
// pgconn.ParseConfig routes each DSN setting one of two ways: a name in its
// notRuntimeParams list becomes a config FIELD, and everything else goes into
// RuntimeParams and rides the startup packet as a session default. A timeout
// that landed in neither would be silently discarded — the DSN string would
// still contain it and every string-level assertion would still pass, which is
// exactly how the Edge ran for months with pragmas its driver ignored.
//
// The docker test proves the server enforces these. This one proves the parse
// step did not drop them, and it fails in the local gate rather than in the
// 20-minute suite.
func TestPgxConnConfig_TimeoutsSurviveDSNParsing(t *testing.T) {
	cc, err := pgxConnConfig(&config.PostgresConfig{
		Host: "localhost", Port: 5432, Database: "shingo",
		User: "u", Password: "p", SSLMode: "disable",
	})
	if err != nil {
		t.Fatalf("pgxConnConfig: %v", err)
	}
	for _, tc := range []struct{ param, want string }{
		{"lock_timeout", "3000"},
		{"statement_timeout", "30000"},
	} {
		if got := cc.RuntimeParams[tc.param]; got != tc.want {
			t.Errorf("RuntimeParams[%s] = %q, want %q — the DSN sets it but the parse dropped it",
				tc.param, got, tc.want)
		}
	}
	// connect_timeout is the counterexample that makes the assertion above
	// meaningful: it IS in notRuntimeParams, so it becomes a field and must NOT
	// appear alongside the two that do.
	if got := cc.RuntimeParams["connect_timeout"]; got != "" {
		t.Errorf("RuntimeParams[connect_timeout] = %q, want absent (it is a config field)", got)
	}
	if cc.ConnectTimeout != connectTimeoutSeconds*time.Second {
		t.Errorf("ConnectTimeout = %s, want %s", cc.ConnectTimeout, connectTimeoutSeconds*time.Second)
	}
}
