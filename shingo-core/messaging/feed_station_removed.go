package messaging

// feed_station_removed.go — the in-memory half of Remove station
// (store.RemoveStaleStation), run after its commit: what this Core holds of the
// station's feeds. A station that comes back is compared, sent and watched as
// one Core has never heard from.

// StationRemoved forgets a removed station's builds, send records, flags and
// quotes, and its bins watch list.
func (s *CoreDataService) StationRemoved(station string) {
	s.feeds.stationRemoved(station)
	s.bins.stationRemoved(station)
}

// stationRemoved forgets one station. A push already armed for it finds no
// send record and no quote, and sends nothing (lastKnown).
func (f *feedState) stationRemoved(station string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drops++
	for _, row := range coreFeeds {
		if row.perStation {
			slot := row.slot(station)
			delete(f.memo, slot)
			f.dropped[slot] = f.drops
		}
	}
	for rk := range f.sent {
		if rk[0] == station {
			delete(f.sent, rk)
		}
	}
	for rk := range f.quoted {
		if rk[0] == station {
			delete(f.quoted, rk)
		}
	}
	delete(f.flags, station)
}

// stationRemoved forgets one station's watch list, its armed push and the
// orders it was watching.
func (b *binsFeed) stationRemoved(station string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.lists, station)
	if t := b.pending[station]; t != nil {
		t.Stop()
		delete(b.pending, station)
	}
	for id, watchers := range b.orders {
		var kept []string
		for _, st := range watchers {
			if st != station {
				kept = append(kept, st)
			}
		}
		if len(kept) == 0 {
			delete(b.orders, id)
		} else {
			b.orders[id] = kept
		}
	}
}
