package bins

import (
	"testing"
	"time"
)

// TestResolveLoadedAt pins the loaded_at resolution: empty falls back to
// now (no error), a valid RFC3339 is normalized to UTC, and anything that
// isn't RFC3339 — including the old zoneless "2006-01-02 15:04:05" layout
// that caused R20-1 — is rejected (error) and falls back to now rather than
// being stored as a zone-skewed instant.
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

	t.Run("zoneless layout (the R20-1 bug input) is rejected, falls back to now", func(t *testing.T) {
		got, err := resolveLoadedAt("2026-06-01 15:04:05", now)
		if err == nil {
			t.Fatal("want error for zoneless (non-RFC3339) input")
		}
		if !got.Equal(now) {
			t.Errorf("got %v, want now %v", got, now)
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
