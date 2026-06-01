package store

import (
	"testing"

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
