// Package eta computes operator-visible ETAs for in-transit orders.
//
// The cache holds a per-route p70 transit time (in_transit → delivered)
// computed over the last 7 days of order history. Refreshed on boot and
// every 10 minutes. Lookups fall back to the global p70 when a route
// has no samples, and to a static default when even the global is empty
// (cold-start, fresh plant).
//
// Why p70: shipping the 70th percentile is a built-in "under-promise"
// pad — 70 % of historical orders arrive in this much time or less,
// which is the Uber/Amazon "feels-early" UX. No additional fudge
// multiplier needed.
//
// Why last-in-transit → first-delivered (not first → first): on a
// two-robot swap the order can transition in_transit → staged →
// in_transit → delivered. The first in_transit includes the partner
// wait, which is operator-irrelevant; the last in_transit is "robot
// moving toward the line" which is the number the operator wants.
package eta

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"time"
)

const (
	// defaultTransit is the fallback when neither a route-specific nor a
	// global p70 is available. Plant orders typically run 60–180 s; the
	// midpoint is a defensible "we don't know yet" answer.
	defaultTransit = 2 * time.Minute

	// outlierCap discards in_transit→delivered durations longer than this
	// from the median calc. Orders that sit in_transit for >1 h are
	// almost always a stuck-robot incident rather than a delivery; they
	// poison the median if included.
	outlierCap = time.Hour

	// windowDays is the rolling history window. 7 days smooths within-
	// week noise and reacts to durable changes (route layout, robot
	// swap) within a week.
	windowDays = 7

	// refreshInterval governs how often the cache is rebuilt. 10 min is
	// faster than plant conditions actually change but cheap enough not
	// to matter.
	refreshInterval = 10 * time.Minute
)

// routeKey identifies a delivery lane. Source + delivery node together
// are enough for the plant's bin-flow geometry; throwing process_node
// in would split the data too thin for most routes.
type routeKey struct {
	source, delivery string
}

// Cache is safe for concurrent use. Refresh swaps the snapshot under a
// write lock; lookups read under a read lock. The whole map is small
// (hundreds of routes at most) so we replace it wholesale rather than
// patching keys.
type Cache struct {
	db *sql.DB

	mu        sync.RWMutex
	routes    map[routeKey]time.Duration
	globalP70 time.Duration
	updatedAt time.Time
}

// NewCache returns an empty cache. Call Refresh before first Lookup, or
// Start to wire the background refresher.
func NewCache(db *sql.DB) *Cache {
	return &Cache{
		db:     db,
		routes: make(map[routeKey]time.Duration),
	}
}

// Lookup returns the padded transit estimate for a (source, delivery)
// pair. The bool reports whether a real number was found:
//   - true:  route-specific p70 in use
//   - false: fell back to the global p70 or to defaultTransit
//
// Callers that want to surface "this is a rougher estimate" UX can
// check the bool; the stamping path ignores it.
func (c *Cache) Lookup(source, delivery string) (time.Duration, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if d, ok := c.routes[routeKey{source, delivery}]; ok {
		return d, true
	}
	if c.globalP70 > 0 {
		return c.globalP70, false
	}
	return defaultTransit, false
}

// Start launches the background refresher. Runs an initial refresh
// synchronously so the first lookup after Start returns real data.
// Returns the refresh error so the engine can log it; the goroutine is
// kicked off unconditionally so a cold-start failure (e.g. fresh DB
// with no completed orders yet) still leaves the cache healthy for
// later refreshes. Stop the loop by closing stopChan.
func (c *Cache) Start(stopChan <-chan struct{}) error {
	ctx := context.Background()
	err := c.Refresh(ctx)
	go c.run(stopChan)
	return err
}

func (c *Cache) run(stopChan <-chan struct{}) {
	t := time.NewTicker(refreshInterval)
	defer t.Stop()
	for {
		select {
		case <-stopChan:
			return
		case <-t.C:
			if err := c.Refresh(context.Background()); err != nil {
				log.Printf("eta: refresh: %v", err)
			}
		}
	}
}

// Refresh rebuilds the cache from order_history. Atomically swaps the
// map under a write lock so concurrent Lookups never see a partial
// snapshot.
func (c *Cache) Refresh(ctx context.Context) error {
	routes, err := c.queryRouteMedians(ctx)
	if err != nil {
		return err
	}
	global, err := c.queryGlobalMedian(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.routes = routes
	c.globalP70 = global
	c.updatedAt = time.Now()
	c.mu.Unlock()
	return nil
}

// queryRouteMedians runs the per-(source, delivery) p70 calc.
//
// For each completed order in the window, we pair the LAST in_transit
// row with the FIRST delivered row in order_history. That delta is the
// final-leg transit duration. PERCENTILE_CONT(0.70) over those deltas
// per route gives the lookup value.
func (c *Cache) queryRouteMedians(ctx context.Context) (map[routeKey]time.Duration, error) {
	const q = `
WITH journeys AS (
    SELECT
        o.id            AS order_id,
        o.source_node,
        o.delivery_node,
        (SELECT MAX(created_at) FROM order_history
            WHERE order_id = o.id AND status = 'in_transit') AS last_in_transit_at,
        (SELECT MIN(created_at) FROM order_history
            WHERE order_id = o.id AND status = 'delivered') AS first_delivered_at
    FROM orders o
    WHERE o.status IN ('delivered', 'confirmed')
      AND o.completed_at >= NOW() - make_interval(days => $1)
)
SELECT
    source_node,
    delivery_node,
    PERCENTILE_CONT(0.70) WITHIN GROUP (
        ORDER BY EXTRACT(EPOCH FROM (first_delivered_at - last_in_transit_at))
    ) AS p70_sec
FROM journeys
WHERE last_in_transit_at IS NOT NULL
  AND first_delivered_at IS NOT NULL
  AND first_delivered_at > last_in_transit_at
  AND first_delivered_at - last_in_transit_at < make_interval(secs => $2)
GROUP BY source_node, delivery_node
`
	rows, err := c.db.QueryContext(ctx, q, windowDays, int(outlierCap.Seconds()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[routeKey]time.Duration)
	for rows.Next() {
		var src, dst string
		var sec float64
		if err := rows.Scan(&src, &dst, &sec); err != nil {
			return nil, err
		}
		if sec <= 0 {
			continue
		}
		out[routeKey{src, dst}] = time.Duration(sec * float64(time.Second))
	}
	return out, rows.Err()
}

// queryGlobalMedian computes the p70 across all routes for the fallback
// ladder. Same window and outlier cap as the per-route query.
func (c *Cache) queryGlobalMedian(ctx context.Context) (time.Duration, error) {
	const q = `
WITH journeys AS (
    SELECT
        (SELECT MAX(created_at) FROM order_history
            WHERE order_id = o.id AND status = 'in_transit') AS last_in_transit_at,
        (SELECT MIN(created_at) FROM order_history
            WHERE order_id = o.id AND status = 'delivered') AS first_delivered_at
    FROM orders o
    WHERE o.status IN ('delivered', 'confirmed')
      AND o.completed_at >= NOW() - make_interval(days => $1)
)
SELECT
    PERCENTILE_CONT(0.70) WITHIN GROUP (
        ORDER BY EXTRACT(EPOCH FROM (first_delivered_at - last_in_transit_at))
    )
FROM journeys
WHERE last_in_transit_at IS NOT NULL
  AND first_delivered_at IS NOT NULL
  AND first_delivered_at > last_in_transit_at
  AND first_delivered_at - last_in_transit_at < make_interval(secs => $2)
`
	var sec sql.NullFloat64
	if err := c.db.QueryRowContext(ctx, q, windowDays, int(outlierCap.Seconds())).Scan(&sec); err != nil {
		return 0, err
	}
	if !sec.Valid || sec.Float64 <= 0 {
		return 0, nil
	}
	return time.Duration(sec.Float64 * float64(time.Second)), nil
}
