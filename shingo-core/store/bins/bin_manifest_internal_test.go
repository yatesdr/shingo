package bins

import (
	"testing"
	"time"
)

// TestResolveLoadedAt pins loaded_at resolution: empty falls back to now (no
// error); an RFC3339 value with an offset is normalized to the correct UTC
// instant; a zoneless value — including the "2006-01-02 15:04:05" layout that
// caused R20-1 — is parsed AS UTC (never re-localized to the session
// TimeZone); and only genuinely unparseable input falls back to now with an
// error.
func TestResolveLoadedAt(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	t.Run("empty falls back to now, no error", func(t *testing.T) {
		got, err := resolveLoadedAt("", now)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if !got.Equal(now) {
			t.Errorf("got %v, want %v", got, now)
		}
	})

	t.Run("RFC3339 with offset is normalized to the correct UTC instant", func(t *testing.T) {
		// 07:00 at -05:00 is 12:00 UTC.
		got, err := resolveLoadedAt("2026-06-01T07:00:00-05:00", now)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		want := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("zoneless space layout (the R20-1 input) is parsed as UTC", func(t *testing.T) {
		got, err := resolveLoadedAt("2024-06-15 12:34:56", now)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		want := time.Date(2024, 6, 15, 12, 34, 56, 0, time.UTC)
		if !got.Equal(want) {
			t.Errorf("got %v, want %v (zoneless must read as UTC, not server-local)", got, want)
		}
	})

	t.Run("zoneless T-separated layout is parsed as UTC", func(t *testing.T) {
		got, err := resolveLoadedAt("2024-06-15T12:34:56", now)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		want := time.Date(2024, 6, 15, 12, 34, 56, 0, time.UTC)
		if !got.Equal(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("garbage is rejected, falls back to now", func(t *testing.T) {
		got, err := resolveLoadedAt("not-a-time", now)
		if err == nil {
			t.Fatal("want error for unparseable input")
		}
		if !got.Equal(now) {
			t.Errorf("got %v, want now %v", got, now)
		}
	})
}
