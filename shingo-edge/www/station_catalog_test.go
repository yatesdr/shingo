package www

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/engine"
)

// station_catalog_test.go — the station says when its payload catalog is old.
//
// The catalog (FeedCatalog) carries each part's manifest lines, which the load
// modal reads. The view says "as of" when the held copy has not been current
// for longer than engine.FeedAsOfAfter, as bins does, and "not known" only
// when Core speaks feeds and no copy is held at all, as containment does.
// Stamped from the feed book's times: memory, no statement.

type catalogFeedFake struct {
	received, confirmed time.Time
	speaks              bool
}

func (f catalogFeedFake) FeedTimes(key string) (time.Time, time.Time) {
	if key != protocol.FeedCatalog {
		return time.Time{}, time.Time{}
	}
	return f.received, f.confirmed
}
func (f catalogFeedFake) CoreSpeaksFeeds() bool { return f.speaks }

func TestStationCatalog_SaysAsOfAndNotKnown(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	old := now.Add(-engine.FeedAsOfAfter - time.Second)
	cases := []struct {
		name string
		fake catalogFeedFake
		want *domain.CatalogKnown
	}{
		{"confirmed just now", catalogFeedFake{received: old, confirmed: now.Add(-time.Minute), speaks: true}, nil},
		{"arrived just now, never confirmed", catalogFeedFake{received: now.Add(-time.Second), speaks: true}, nil},
		{"at the threshold", catalogFeedFake{received: now.Add(-engine.FeedAsOfAfter), speaks: true}, nil},
		{"not current for longer than the threshold", catalogFeedFake{received: old.Add(-time.Hour), confirmed: old, speaks: true},
			&domain.CatalogKnown{Known: true, AsOf: &old}},
		{"nothing held, Core speaks feeds", catalogFeedFake{speaks: true}, &domain.CatalogKnown{Known: false}},
		{"nothing held, an older Core", catalogFeedFake{}, nil},
	}
	for _, c := range cases {
		v := &domain.OperatorStationView{}
		stampStationCatalog(c.fake, v, now)
		got, want := v.Catalog, c.want
		switch {
		case got == nil && want == nil:
		case got == nil || want == nil:
			t.Errorf("%s: catalog %+v, want %+v", c.name, got, want)
		case got.Known != want.Known || (got.AsOf == nil) != (want.AsOf == nil) || (got.AsOf != nil && !got.AsOf.Equal(*want.AsOf)):
			t.Errorf("%s: catalog %+v, want %+v", c.name, got, want)
		}
	}
	// An engine with no feed book (a handler test's stub) stamps nothing.
	v := &domain.OperatorStationView{}
	stampStationCatalog(struct{}{}, v, now)
	if v.Catalog != nil {
		t.Errorf("an engine with no feed times stamped %+v", v.Catalog)
	}
}
