//go:build docker

package store_test

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// demand_analytics_docker_test.go — the Postgres-side guards on Stage 5.2's
// child-outcome tally and Stage 5.7's orphan reads.
//
// WHAT THESE COVER THAT THE PURE TESTS CANNOT. The view tests hold every
// classification rule without a database, which is why they exist. What they
// cannot reach is whether the SQL that FEEDS them says what the Go thinks it
// says: whether the reached-vendor boolean survives the round trip, whether the
// bucket arithmetic lands a row in the bucket it belongs to, and whether a NULL
// aggregate scans as nil rather than as a zero time. Each of those is a place a
// correct renderer would still render the wrong thing.

func insertOrder(t *testing.T, db *store.DB, station, status, vendorID, originClass string,
	originID any, created time.Time, agedAt *time.Time) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO orders (edge_uuid, station_id, status, vendor_order_id,
		                    origin_id, origin_class, created_at, orphan_aged_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		"uuid-"+status+"-"+created.Format("150405.000000000"), station, status,
		vendorID, originID, originClass, created, agedAt); err != nil {
		t.Fatalf("insert order: %v", err)
	}
}

// TestCountChildrenByStatus_KeepsTheVendorSplit is the store half of the
// re-arm proxy.
//
// The proxy's entire content is "did the fleet vendor ever acknowledge this
// order", carried as a not-equal-to-empty-string test on vendor_order_id. If
// that expression is wrong, or
// the GROUP BY collapses the two cases, the classifier is handed a single
// bucket and reports every cancelled order as pre-dispatch churn — with every
// pure test still green, because the pure tests are given the split already
// made.
func TestCountChildrenByStatus_KeepsTheVendorSplit(t *testing.T) {
	db := testdb.Open(t)
	now := time.Now().UTC()
	originID := "bbbbbbbb-0000-0000-0000-000000000001"

	o := originAt("cell|PLANT.LINE9|9|PANEL-Z|supply", originID, 1, now.Add(-time.Hour))
	if err := db.UpsertDemandOrigin(o); err != nil {
		t.Fatalf("insert episode: %v", err)
	}

	// Two cancels either side of the vendor handoff, plus a confirmed order
	// that HAS a vendor id — so a query that keyed on the id alone rather than
	// on (status, id) would also be caught.
	insertOrder(t, db, "PLANT.LINE9", string(protocol.StatusCancelled), "", "attached", originID, now, nil)
	insertOrder(t, db, "PLANT.LINE9", string(protocol.StatusCancelled), "", "attached", originID, now.Add(time.Millisecond), nil)
	insertOrder(t, db, "PLANT.LINE9", string(protocol.StatusCancelled), "RDS-9", "attached", originID, now.Add(2*time.Millisecond), nil)
	insertOrder(t, db, "PLANT.LINE9", string(protocol.StatusConfirmed), "RDS-10", "attached", originID, now.Add(3*time.Millisecond), nil)

	counts, err := db.CountChildrenByStatus(now.Add(-2 * time.Hour))
	if err != nil {
		t.Fatalf("count children: %v", err)
	}

	var earlyCancels, lateCancels, confirmed int
	for _, c := range counts {
		if c.OriginID != originID {
			continue
		}
		switch {
		case c.Status == string(protocol.StatusCancelled) && !c.ReachedVendor:
			earlyCancels += c.Count
		case c.Status == string(protocol.StatusCancelled) && c.ReachedVendor:
			lateCancels += c.Count
		case c.Status == string(protocol.StatusConfirmed):
			confirmed += c.Count
		}
	}

	if earlyCancels != 2 || lateCancels != 1 {
		t.Errorf("cancels split early=%d late=%d, want 2 and 1.\n"+
			"The (vendor_order_id <> '') expression is the ONLY thing separating a "+
			"pre-dispatch cancel from a post-dispatch one; collapsed, every cancelled "+
			"order reports as re-arm churn.", earlyCancels, lateCancels)
	}
	if confirmed != 1 {
		t.Errorf("confirmed = %d, want 1", confirmed)
	}
}

// TestCountChildrenByStatus_ScopeMatchesTheBrowser holds the two queries to one
// window.
//
// The browser shows "open, plus anything that closed inside the window". If
// this query used a different predicate, rows would appear on the page whose
// cause column had no data through no fault of the data — an absence that is an
// artefact of two queries disagreeing about what they were asked.
func TestCountChildrenByStatus_ScopeMatchesTheBrowser(t *testing.T) {
	db := testdb.Open(t)
	now := time.Now().UTC()
	since := now.Add(-time.Hour)

	// Inside: still open. Outside: closed well before the window opened.
	openID := "bbbbbbbb-0000-0000-0000-000000000002"
	staleID := "bbbbbbbb-0000-0000-0000-000000000003"

	// OPENED EIGHT HOURS AGO AND STILL OPEN — deliberately older than the
	// window. This is the case the browser's own comment calls the single most
	// important row it can show, and it is the only fixture that can tell
	// "closed_at IS NULL OR closed_at >= since" apart from "opened_at >= since":
	// a recently-opened episode satisfies both, so it proves nothing.
	live := originAt("cell|PLANT.L1|1|P|supply", openID, 1, now.Add(-8*time.Hour))
	if err := db.UpsertDemandOrigin(live); err != nil {
		t.Fatalf("insert open episode: %v", err)
	}
	closedLongAgo := now.Add(-6 * time.Hour)
	stale := originAt("cell|PLANT.L2|2|P|supply", staleID, 1, now.Add(-8*time.Hour))
	stale.ClosedAt = &closedLongAgo
	stale.CloseReason = "recovered"
	if err := db.UpsertDemandOrigin(stale); err != nil {
		t.Fatalf("insert stale episode: %v", err)
	}

	insertOrder(t, db, "PLANT.L1", string(protocol.StatusConfirmed), "v1", "attached", openID, now, nil)
	insertOrder(t, db, "PLANT.L2", string(protocol.StatusConfirmed), "v2", "attached", staleID, now, nil)

	counts, err := db.CountChildrenByStatus(since)
	if err != nil {
		t.Fatalf("count children: %v", err)
	}
	seen := map[string]bool{}
	for _, c := range counts {
		seen[c.OriginID] = true
	}
	if !seen[openID] {
		t.Error("an OPEN episode's children were not counted. Open episodes are never " +
			"windowed out of the browser, so windowing them out here leaves a visible " +
			"row with an empty cause column.")
	}
	if seen[staleID] {
		t.Error("an episode closed six hours before the window still had its children " +
			"counted — the two queries disagree about the window")
	}
}

// TestOrphanRateBuckets_BucketsOnCreatedAt is the timestamp decision, checked.
//
// The trend is keyed on orders.created_at and NOT on orphan_aged_at. See
// domain.OrphanBucket for the four reasons; the one this test can demonstrate
// is the second: orphan_aged_at is NULL on every orphan younger than the
// sweep's grace period, so a trend keyed on it is structurally blind to the
// recent past — exactly the buckets that would show a rate starting to climb.
func TestOrphanRateBuckets_BucketsOnCreatedAt(t *testing.T) {
	db := testdb.Open(t)
	// A fixed hour boundary so bucket membership is unambiguous.
	base := time.Now().UTC().Truncate(time.Hour).Add(-4 * time.Hour)

	// Hour 0: 1 orphan (never aged — the sweep has not reached it) + 3 normals.
	insertOrder(t, db, "S1", "confirmed", "v", protocol.OriginClassOrphan, nil, base.Add(5*time.Minute), nil)
	for i := 0; i < 3; i++ {
		insertOrder(t, db, "S1", "confirmed", "v", "attached", nil,
			base.Add(time.Duration(10+i)*time.Minute), nil)
	}
	// Hour 2: 2 orphans, one of them aged, + 2 normals.
	h2 := base.Add(2 * time.Hour)
	aged := h2.Add(time.Hour)
	insertOrder(t, db, "S1", "confirmed", "v", protocol.OriginClassOrphan, nil, h2.Add(time.Minute), &aged)
	insertOrder(t, db, "S1", "confirmed", "v", protocol.OriginClassOrphan, nil, h2.Add(2*time.Minute), nil)
	for i := 0; i < 2; i++ {
		insertOrder(t, db, "S1", "confirmed", "v", "attached", nil,
			h2.Add(time.Duration(5+i)*time.Minute), nil)
	}

	buckets, err := db.OrphanRateBuckets(base.Add(-time.Minute), time.Hour)
	if err != nil {
		t.Fatalf("orphan rate buckets: %v", err)
	}

	byHour := map[time.Time]store.OrphanBucket{}
	for _, b := range buckets {
		byHour[b.Start.UTC()] = b
	}

	if got := byHour[base]; got.Orphans != 1 || got.Orders != 4 {
		t.Errorf("hour 0: orphans=%d orders=%d, want 1 and 4. The denominator must be "+
			"EVERY order in the bucket, not just the orphaned ones — a rate over its "+
			"own numerator is always 100%%.", got.Orphans, got.Orders)
	}
	if got := byHour[h2]; got.Orphans != 2 || got.Orders != 4 {
		t.Errorf("hour 2: orphans=%d orders=%d, want 2 and 4.\n"+
			"BOTH orphans must count — one of them has orphan_aged_at set and one does "+
			"not, and a trend that only saw the aged one would be blind to every orphan "+
			"younger than the sweep's grace period.", got.Orphans, got.Orders)
	}

	// Hour 1 had nothing at all. A GROUP BY cannot emit it, and that ABSENCE is
	// what www.BuildOrphanTrend turns into an unmeasured bucket. If the query
	// started emitting a zero row here the view would render 0% for an hour
	// nobody measured.
	if _, present := byHour[base.Add(time.Hour)]; present {
		t.Error("the query emitted a bucket with no rows. Empty buckets must be ABSENT " +
			"here so the view can render them as unmeasured rather than as a rate of zero.")
	}
}

// TestSummarizeOrphansBySite_NullOldestLiveScansAsNil guards the one nullable
// aggregate.
//
// MIN(created_at) FILTER (WHERE orphan_aged_at IS NULL) returns NULL for a
// station whose findings have all aged out. Scanned into a time.Time that would
// be the zero time, which renders as a real date in the year 1 — and the view's
// "no live findings" branch, which keys on nil, would never fire. A cleaned-up
// station would read as one with a 2000-year-old open finding.
func TestSummarizeOrphansBySite_NullOldestLiveScansAsNil(t *testing.T) {
	db := testdb.Open(t)
	now := time.Now().UTC()
	agedAt := now.Add(-time.Hour)

	// LIVE-ONE: two live findings and one aged. LIVE-NONE: all aged.
	oldest := now.Add(-3 * time.Hour)
	insertOrder(t, db, "LIVE-ONE", "confirmed", "v", protocol.OriginClassOrphan, nil, oldest, nil)
	insertOrder(t, db, "LIVE-ONE", "confirmed", "v", protocol.OriginClassOrphan, nil, now.Add(-time.Minute), nil)
	insertOrder(t, db, "LIVE-ONE", "confirmed", "v", protocol.OriginClassOrphan, nil, now.Add(-4*time.Hour), &agedAt)
	insertOrder(t, db, "LIVE-NONE", "confirmed", "v", protocol.OriginClassOrphan, nil, now.Add(-5*time.Hour), &agedAt)

	sites, err := db.SummarizeOrphansBySite()
	if err != nil {
		t.Fatalf("summarize orphans by site: %v", err)
	}
	byStation := map[string]store.OrphanSite{}
	for _, s := range sites {
		byStation[s.StationID] = s
	}

	none := byStation["LIVE-NONE"]
	if none.OldestLive != nil {
		t.Errorf("a station with no live findings scanned OldestLive as %v, want nil.\n"+
			"A zero time renders as a real date and the view's not-applicable branch "+
			"never fires — a cleaned-up station would read as one holding a very old "+
			"open finding.", *none.OldestLive)
	}
	if none.Live != 0 || none.Aged != 1 || none.Total != 1 {
		t.Errorf("LIVE-NONE: live=%d aged=%d total=%d, want 0/1/1",
			none.Live, none.Aged, none.Total)
	}

	one := byStation["LIVE-ONE"]
	if one.OldestLive == nil {
		t.Fatal("a station with two live findings scanned OldestLive as nil")
	}
	if !one.OldestLive.Equal(oldest.Truncate(time.Microsecond)) {
		t.Errorf("OldestLive = %s, want the OLDEST live finding %s — a MIN over all "+
			"findings rather than over the live ones would report an aged row's age",
			one.OldestLive.UTC(), oldest.Truncate(time.Microsecond))
	}
	if one.Live != 2 || one.Aged != 1 || one.Total != 3 {
		t.Errorf("LIVE-ONE: live=%d aged=%d total=%d, want 2/1/3 — Live + Aged must "+
			"equal Total or the lane is not trustworthy", one.Live, one.Aged, one.Total)
	}

	// Worst first: the station with live findings sorts above the one without.
	if len(sites) >= 2 && sites[0].StationID != "LIVE-ONE" {
		t.Errorf("lane head is %q, want LIVE-ONE — live findings are the actionable "+
			"ones and belong at the top", sites[0].StationID)
	}
}
