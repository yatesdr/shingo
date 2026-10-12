package engine

import (
	"testing"
	"time"

	"shingocore/domain"
)

// forecastClose's three answers (D2.2), pure.
func TestForecastClose(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { x := now.Add(d); return &x }
	for _, c := range []struct {
		name string
		rec  domain.ForecastRecord
		cur  *time.Time
		want string
	}{
		{"open is never closed here", domain.ForecastRecord{Open: true, NeedAt: at(-time.Hour)}, nil, ""},
		{"unprojectable keeps", domain.ForecastRecord{}, nil, ""},
		{"past need and grace: expired", domain.ForecastRecord{NeedAt: at(-forecastExpiryGrace - time.Minute)}, nil, domain.CloseReasonExpired},
		{"past need within grace keeps", domain.ForecastRecord{NeedAt: at(-time.Minute)}, at(-time.Minute), ""},
		{"stock rose: need moved later past the band", domain.ForecastRecord{NeedAt: at(10 * time.Minute)}, at(40 * time.Minute), domain.CloseReasonRecoveredUnattributed},
		{"need moved later within the band keeps", domain.ForecastRecord{NeedAt: at(10 * time.Minute)}, at(10*time.Minute + 30*time.Second), ""},
		{"need moved earlier keeps (an update)", domain.ForecastRecord{NeedAt: at(30 * time.Minute)}, at(5 * time.Minute), ""},
		// A line at or below its level projects now (max(0, remaining-level)),
		// and now slides with every pass while its supply is in flight. That is
		// a place that is due, not stock that rose. RED at 3d8dce49: the after
		// run closed 404 forecasts this way on 17 places.
		{"due now is not a recovery", domain.ForecastRecord{NeedAt: at(-3 * time.Minute)}, at(0), ""},
	} {
		if got := forecastClose(c.rec, c.cur, now); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
