// core_health.go — the Core Health strip's data source.
//
// None of this existed: there is no runtime.NumGoroutine, no ReadMemStats and
// no sql.DBStats anywhere else in shingo-core. Core reported every EDGE's
// health and could not state its own.
//
// HEALTH IS A VERDICT, NOT A STAT DUMP. One derived line answers "is Core OK";
// the numbers are EVIDENCE for it and stay quiet until they are the reason it
// is not green. Same principle as the exception list — silent on a good day.
//
// Deliberately excluded: heap MB, GC count, alloc rate. The value is not
// actionable and only the trend is, which goroutines already carry. Process
// CPU% via /proc/self/stat deltas is skipped too — load average answers nearly
// the same question for one file read.

package www

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
)

// buildInfo is stamped once at boot by the composition root. Version and
// Commit come from -ldflags (see shingo-core/Makefile and install-core.sh);
// left at their defaults a binary cannot say which commit it is, which is what
// made the five Springfield restarts on 2026-07-24 impossible to attribute.
var buildInfo = struct {
	mu      sync.RWMutex
	version string
	commit  string
	bootAt  time.Time
}{version: "dev", commit: "unknown", bootAt: time.Now()}

// SetBuildInfo records the running build. Called once from main.
func SetBuildInfo(version, commit string, bootAt time.Time) {
	buildInfo.mu.Lock()
	defer buildInfo.mu.Unlock()
	buildInfo.version, buildInfo.commit, buildInfo.bootAt = version, commit, bootAt
}

// goroutineRing is the 12-slot in-memory history behind the sparkline.
//
// Twelve points, no schema, resets on restart — and that is the whole design.
// The goroutine COUNT is meaningless to a reader; the DIRECTION is the leak
// signal, and twelve samples is enough to see a slope. Persisting it would buy
// nothing a restart-surviving trend could act on.
var goroutineRing = struct {
	mu     sync.Mutex
	points []int
}{}

const goroutineRingSize = 12

// dbWaitGauge turns sql.DBStats.WaitCount into a gauge.
//
// WaitCount is a CUMULATIVE LIFETIME counter — the one counter among four
// otherwise-true gauges (dead letters, completion anomalies, load, dependency
// pings). Comparing it to zero meant the FIRST pool wait ever recorded pinned
// the strip amber until Core restarted, and a verdict that latches is not a
// verdict. Hold the previous sample; report the waits since it.
//
// The baseline advances at most once per window rather than on every call
// because the strip is polled per client: advancing on every read would split
// one wait across however many dashboards happen to be open, so two operators
// watching the same Core would see different verdicts.
var dbWaitGauge = struct {
	mu         sync.Mutex
	started    bool
	baseline   int64
	baselineAt time.Time
	reported   int64
}{}

// dbWaitWindow matches the strip's poll interval (dashboard-landing.js).
const dbWaitWindow = 15 * time.Second

// expiredDropGauge windows protocol.ExpiredDrops the same way dbWaitGauge
// windows sql.DBStats.WaitCount, and for the same reason: the underlying number
// is a process-lifetime total, so reporting it raw would latch the verdict
// degraded forever after a single bad afternoon.
var expiredDropGauge = struct {
	mu         sync.Mutex
	started    bool
	baseline   int64
	baselineAt time.Time
	reported   int64
}{}

// expiredDropWindow is deliberately far longer than dbWaitWindow. Expired drops
// arrive in bursts at reconnect — Springfield saw 124 in one minute and then
// nothing for an hour — so a 15-second window would show zero almost always and
// miss the event entirely. Five minutes is long enough to still be showing the
// burst when someone opens the dashboard because of it.
const expiredDropWindow = 5 * time.Minute

// expiredDropsSinceBaseline reports drops in the last completed window. Same
// contract as waitsSinceBaseline: closed-window delta so concurrent pollers
// agree, zero on first observation so startup does not report history as a
// symptom, and a re-baseline if the total ever goes backwards.
func expiredDropsSinceBaseline(total int64, now time.Time) int64 {
	expiredDropGauge.mu.Lock()
	defer expiredDropGauge.mu.Unlock()
	if !expiredDropGauge.started || total < expiredDropGauge.baseline {
		expiredDropGauge.started = true
		expiredDropGauge.baseline, expiredDropGauge.baselineAt, expiredDropGauge.reported = total, now, 0
		return 0
	}
	if now.Sub(expiredDropGauge.baselineAt) >= expiredDropWindow {
		expiredDropGauge.reported = total - expiredDropGauge.baseline
		expiredDropGauge.baseline, expiredDropGauge.baselineAt = total, now
	}
	return expiredDropGauge.reported
}

// waitsSinceBaseline reports the waits recorded in the last completed window.
//
// The value is the CLOSED window's delta, not the open one's, so every client
// polling inside a window sees the same number instead of racing each other
// for the increments. The first observation returns 0: at the first poll of a
// Core that has been up for a week the lifetime total is history, not a
// symptom, and reporting it would reproduce the latch for one window. A total
// that goes backwards — a pool replaced underneath us — re-baselines rather
// than reporting a negative.
func waitsSinceBaseline(total int64, now time.Time) int64 {
	dbWaitGauge.mu.Lock()
	defer dbWaitGauge.mu.Unlock()
	if !dbWaitGauge.started || total < dbWaitGauge.baseline {
		dbWaitGauge.started = true
		dbWaitGauge.baseline, dbWaitGauge.baselineAt, dbWaitGauge.reported = total, now, 0
		return 0
	}
	if now.Sub(dbWaitGauge.baselineAt) >= dbWaitWindow {
		dbWaitGauge.reported = total - dbWaitGauge.baseline
		dbWaitGauge.baseline, dbWaitGauge.baselineAt = total, now
	}
	return dbWaitGauge.reported
}

// sampleGoroutines appends the current count and returns the window,
// oldest first.
func sampleGoroutines() []int {
	n := runtime.NumGoroutine()
	goroutineRing.mu.Lock()
	defer goroutineRing.mu.Unlock()
	goroutineRing.points = append(goroutineRing.points, n)
	if len(goroutineRing.points) > goroutineRingSize {
		goroutineRing.points = goroutineRing.points[len(goroutineRing.points)-goroutineRingSize:]
	}
	return append([]int(nil), goroutineRing.points...)
}

// CoreHealth is the strip's payload. Verdict first, evidence after — the
// ordering is the design.
type CoreHealth struct {
	// Verdict is "ok" or "degraded". Worst-of across the Reasons below.
	Verdict string `json:"verdict"`
	// Reasons is why it is degraded, in the order found. Empty when ok.
	Reasons []string `json:"reasons"`
	// DepsDown names the dependencies that are down, and ONLY those.
	//
	// The strip used to print all four dots permanently beside a verdict that
	// is derived from them — the conclusion and its evidence side by side,
	// saying the same thing twice and costing a whole zone's width to do it.
	// When everything is up this is empty and the strip shows the verdict
	// alone; when something is down the strip names that one thing.
	DepsDown []string `json:"deps_down"`

	Version   string `json:"version"`
	Commit    string `json:"commit"`
	UptimeSec int64  `json:"uptime_sec"`
	// Uptime is preformatted "0d 00h 14m" — beside a red verdict it reads as
	// "it broke right after a restart" with no correlation work. This is where
	// the build stamp becomes visible rather than merely logged.
	Uptime string `json:"uptime"`

	// DB pool. A ratio against a limit — a meter, not a number.
	DBInUse   int `json:"db_in_use"`
	DBIdle    int `json:"db_idle"`
	DBMaxOpen int `json:"db_max_open"`
	// DBWaitsRecent is waits in the last completed window, NOT the lifetime
	// total sql.DBStats reports. See dbWaitGauge.
	DBWaitsRecent int64 `json:"db_waits_recent"`

	// Load average against core count. Also a ratio against a limit.
	Load1 float64 `json:"load1"`
	Cores int     `json:"cores"`

	// Goroutines: the value is noise, the slope is the signal.
	Goroutines       int   `json:"goroutines"`
	GoroutineHistory []int `json:"goroutine_history"`

	// ExpiredDropsRecent is envelopes discarded for expiry in the last
	// completed window — the SECOND silent loss channel, and the larger one.
	// Core's ingestor drops an expired envelope before any handler runs while
	// the sender records a successful publish, so this number is the only place
	// the loss is visible at all. ExpiredDropsTotal is the process lifetime, so
	// a quiet window can still say how much has been lost since boot.
	ExpiredDropsRecent int64 `json:"expired_drops_recent"`
	ExpiredDropsTotal  int64 `json:"expired_drops_total"`

	// Already computed by the reconciliation loop; surfaced rather than
	// recomputed.
	DeadLetters int `json:"dead_letters"`
	// CompletionAnomalies is the WINDOWED count — the one the verdict reads.
	CompletionAnomalies int `json:"completion_anomalies"`
	// CompletionAnomaliesTotal is every one on record, so a green strip can
	// still say "and there are 10 older ones" rather than implying none exist.
	CompletionAnomaliesTotal int `json:"completion_anomalies_total"`
	// CompletionAnomalyWindowHours comes off the reconciliation summary rather
	// than from the constant directly: www may not import the store (depguard
	// 'www-no-direct-store'), and carrying the number on the payload keeps the
	// sentence and the rule that produced it from drifting apart anyway.
	CompletionAnomalyWindowHours int `json:"completion_anomaly_window_hours"`
	SSEClients                   int `json:"sse_clients"`

	// FaultedNow is every order sitting in `faulted` right now; FaultedNotice
	// is the subset past the config threshold.
	//
	// Both come from the DB (ListActiveOrders), not from Poller.FaultedCount() —
	// which has no caller, and is poller MEMORY: after a Core restart it reads
	// zero until the next FAILED transition, because Track seeds StateCreated.
	// A gauge that says "0 faulted" while the board shows three is worse than
	// no gauge. Reading the DB also means the strip and the board agree by
	// construction.
	//
	// ONLY FaultedNotice colours the verdict. 706 of 730 faults over 30 days
	// recovered on their own with a median of 20 seconds; a strip that goes
	// amber for those is amber most of the day and read by nobody.
	FaultedNow    int `json:"faulted_now"`
	FaultedNotice int `json:"faulted_notice"`
	// FaultNoticeAfterSeconds is the threshold behind FaultedNotice, carried so
	// the sentence can name it rather than asserting a bare number of seconds.
	FaultNoticeAfterSeconds int `json:"fault_notice_after_seconds"`
}

const (
	verdictOK       = "ok"
	verdictDegraded = "degraded"
)

// coreHealth assembles the strip.
//
// The verdict is worst-of: any dependency down, DB pool waits > 0, load above
// core count, dead letters present, completion anomalies present. Each has a
// reason string, because a red dot with no sentence is a puzzle, not a signal.
func (h *Handlers) coreHealth(depsOK bool, depReasons []string) CoreHealth {
	buildInfo.mu.RLock()
	version, commit, bootAt := buildInfo.version, buildInfo.commit, buildInfo.bootAt
	buildInfo.mu.RUnlock()

	// WALL clock, deliberately, not clock.Now(). Process uptime is a
	// wall-clock fact: under the sim's 10x fast-forward clock.Now() made
	// seven real minutes read as "0d 06h 07m", which is exactly the kind of
	// wrong number that makes a restart marker useless.
	uptime := time.Since(bootAt)
	if uptime < 0 {
		uptime = 0
	}

	c := CoreHealth{
		Verdict:          verdictOK,
		Version:          version,
		Commit:           commit,
		UptimeSec:        int64(uptime.Seconds()),
		Uptime:           formatUptime(uptime),
		Cores:            runtime.NumCPU(),
		Goroutines:       runtime.NumGoroutine(),
		GoroutineHistory: sampleGoroutines(),
		SSEClients:       h.eventHub.ClientCount(),
	}

	if st, ok := h.engine.HealthService().PoolStats(); ok {
		c.DBInUse, c.DBIdle = st.InUse, st.Idle
		c.DBMaxOpen = st.MaxOpenConnections
		// WALL clock again, for the same reason as uptime above: the window
		// this gauge measures is a real-time one and must not be fast-forwarded
		// by the sim.
		c.DBWaitsRecent = waitsSinceBaseline(st.WaitCount, time.Now())
	}
	c.Load1 = loadAverage1()

	// WALL clock, same reason as the DB gauge above: a real-time window must
	// not be fast-forwarded by the sim.
	c.ExpiredDropsTotal = protocol.ExpiredDrops()
	c.ExpiredDropsRecent = expiredDropsSinceBaseline(c.ExpiredDropsTotal, time.Now())

	if recon, err := h.engine.Reconciliation().Summary(); err == nil && recon != nil {
		c.DeadLetters = recon.DeadLetters
		c.CompletionAnomalies = recon.CompletionAnomalies
		c.CompletionAnomaliesTotal = recon.CompletionAnomaliesTotal
		c.CompletionAnomalyWindowHours = recon.CompletionAnomalyWindowHours
	}

	c.FaultedNow, c.FaultedNotice, c.FaultNoticeAfterSeconds = h.faultedGauge()

	if !depsOK {
		c.DepsDown = depReasons
	}
	c.Reasons = deriveReasons(c, c.DepsDown)
	if len(c.Reasons) > 0 {
		c.Verdict = verdictDegraded
	}
	return c
}

// deriveReasons is the verdict rule: worst-of across dependencies, DB pool
// waits, load, dead letters and completion anomalies.
//
// Every condition produces a SENTENCE, not a flag. A red dot with no
// explanation is a puzzle rather than a signal, and the strip shows the first
// of these under the verdict.
//
// Split out from coreHealth so the rule is testable without standing up an
// engine — the plumbing that gathers the numbers is not what can be wrong here.
func deriveReasons(c CoreHealth, depsDown []string) []string {
	var reasons []string
	reasons = append(reasons, depsDown...)

	// A wait means a request QUEUED for a connection: the pool is the
	// bottleneck, not the database. RECENT waits — the lifetime total this is
	// derived from never goes down, so testing that against zero latched the
	// verdict degraded for the life of the process.
	if c.DBWaitsRecent > 0 {
		reasons = append(reasons, fmt.Sprintf("DB pool waits: %d in %s", c.DBWaitsRecent, dbWaitWindow))
	}
	// Strictly greater: a box at exactly its core count is fully used, not
	// overloaded, and flagging that would cry wolf on every busy plant.
	// Load1 == 0 means /proc was unreadable (Windows, macOS) — not a fault.
	if c.Cores > 0 && c.Load1 > float64(c.Cores) {
		reasons = append(reasons, fmt.Sprintf("load %.2f over %d cores", c.Load1, c.Cores))
	}
	if c.DeadLetters > 0 {
		reasons = append(reasons, fmt.Sprintf("%d %s", c.DeadLetters,
			plural(c.DeadLetters, "dead letter", "dead letters")))
	}
	// Windowed like the DB waits, and named so the sentence points somewhere.
	// An expired drop is almost always transport: the message sat in a sender's
	// outbox past its TTL and Core threw it away on arrival.
	if c.ExpiredDropsRecent > 0 {
		reasons = append(reasons, fmt.Sprintf("%d expired %s dropped in the last %s",
			c.ExpiredDropsRecent,
			plural(int(c.ExpiredDropsRecent), "message", "messages"), expiredDropWindow))
	}
	// Named, windowed, and it says which. "Core degraded" on its own sent a
	// reader looking for a dependency outage; the condition is an order-
	// completion anomaly and the sentence now says so, with the window
	// attached so nobody has to guess whether it means "now" or "ever".
	if c.CompletionAnomalies > 0 {
		reasons = append(reasons, fmt.Sprintf("%d order-completion %s in the last %s",
			c.CompletionAnomalies,
			plural(c.CompletionAnomalies, "anomaly", "anomalies"),
			formatWindow(c.CompletionAnomalyWindowHours)))
	}
	// NOTICE faults only. A 20-second replan is the overwhelming majority of
	// faults and is not a degraded core; colouring the verdict for it would
	// leave the strip amber most of the day.
	if c.FaultedNotice > 0 {
		reasons = append(reasons, fmt.Sprintf("%d %s faulted over %ds",
			c.FaultedNotice,
			plural(c.FaultedNotice, "order", "orders"),
			c.FaultNoticeAfterSeconds))
	}
	return reasons
}

// faultedGauge counts the orders sitting in `faulted` right now, and the subset
// past the notice threshold.
//
// A read failure reports zeros. The strip is a health surface: it must not
// itself become the outage, and a missing gauge is visible next to the
// dependency dots that would also be down.
func (h *Handlers) faultedGauge() (now, notice, thresholdS int) {
	noticeAfter := h.engine.AppConfig().RDS.FaultNoticeAfter
	thresholdS = int(noticeAfter.Seconds())
	svc := h.engine.OrderService()
	orders, err := svc.ListActiveOrders()
	if err != nil {
		log.Printf("core health: list active orders: %v", err)
		return 0, 0, thresholdS
	}
	at := clock.Now().UTC()
	for _, o := range orders {
		if o.Status != protocol.StatusFaulted {
			continue
		}
		now++
		if noticeAfter <= 0 {
			continue
		}
		if hrow, herr := svc.LatestOrderHistoryForStatus(o.ID, protocol.StatusFaulted); herr == nil &&
			hrow != nil && at.Sub(hrow.CreatedAt) >= noticeAfter {
			notice++
		}
	}
	return now, notice, thresholdS
}

// plural picks the singular or plural WORD for n. Two call sites were writing
// "anomal(ies)" and "letter(s)" — a parenthesis is what you print when you do
// not know the count, and here it is always known.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// formatWindow renders the anomaly window for the reason sentence. Takes hours
// rather than a Duration because the value arrives on the summary payload —
// see CompletionAnomalyWindowHours for why it is not read off the constant.
func formatWindow(hours int) string {
	if hours <= 0 {
		return "the window"
	}
	return fmt.Sprintf("%dh", hours)
}

// formatUptime renders the largest meaningful unit and the one below it.
//
// "0d 00h 06m" spends eight characters saying nothing: two zero segments a
// reader has to skip past to reach the only number that matters. "6m" is the
// same fact. This sits beside the build stamp, where the job is to make a
// recent restart legible at a glance — and a wall of zeroes is the opposite.
func formatUptime(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	mins := int(d.Minutes())
	if mins < 60 {
		return fmt.Sprintf("%dm", mins)
	}
	hours, m := mins/60, mins%60
	if hours < 24 {
		if m == 0 {
			return fmt.Sprintf("%dh", hours)
		}
		return fmt.Sprintf("%dh %dm", hours, m)
	}
	days, h := hours/24, hours%24
	if h == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd %dh", days, h)
}

// loadAverage1 reads the 1-minute load average from /proc/loadavg.
//
// Returns 0 where /proc does not exist — Windows and macOS dev machines. The
// strip renders the meter as unavailable rather than claiming an idle box,
// because "0.00 load" and "we cannot read load" are different facts.
func loadAverage1() float64 {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return v
}

// apiCoreHealth serves the strip for the dashboard's SSE-driven refresh.
func (h *Handlers) apiCoreHealth(w http.ResponseWriter, r *http.Request) {
	ok, reasons := h.dependencyState()
	h.jsonOK(w, h.coreHealth(ok, reasons))
}

// dependencyState reports whether every dependency is up, and names the ones
// that are not.
func (h *Handlers) dependencyState() (bool, []string) {
	var down []string
	if err := h.engine.Fleet().Ping(); err != nil {
		down = append(down, "fleet down")
	}
	if !h.engine.MsgClient().IsConnected() {
		down = append(down, "messaging down")
	}
	if h.engine.HealthService().PingDB() != nil {
		down = append(down, "database down")
	}
	return len(down) == 0, down
}
