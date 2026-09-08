package config

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"slices"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"shingocore/cms/client"
	"shingocore/cms/poster"
	"shingocore/cms/wire"
)

// defaultFaultNoticeAfter is the shipped fault-notice threshold, named because
// Defaults() and Load()'s fallback must not be able to disagree.
const defaultFaultNoticeAfter = 60 * time.Second

// defaultStrandedSweepWindow is the shipped age limit on the stranded-bin
// inference, named for the same reason as the constant above: Defaults() and
// Load()'s fallback must not be able to disagree. See
// RDSConfig.StrandedSweepWindow for why the sweep declines older bins.
const defaultStrandedSweepWindow = 2 * time.Hour

type Config struct {
	mu sync.RWMutex `yaml:"-"`

	Database      DatabaseConfig      `yaml:"database"`
	RDS           RDSConfig           `yaml:"rds"`
	Web           WebConfig           `yaml:"web"`
	Messaging     MessagingConfig     `yaml:"messaging"`
	Staging       StagingConfig       `yaml:"staging"`
	FireAlarm     FireAlarmConfig     `yaml:"fire_alarm"`
	Notifications NotificationsConfig `yaml:"notifications"`
	Sim           SimConfig           `yaml:"sim"`
	Sourceability SourceabilityConfig `yaml:"sourceability"`
	Replenishment ReplenishmentConfig `yaml:"replenishment"`
	Logging       LoggingConfig       `yaml:"logging"`
	Dispatch      DispatchConfig      `yaml:"dispatch"`
	Demand        DemandConfig        `yaml:"demand"`

	RobotConfidence RobotConfidenceConfig `yaml:"robot_confidence"`

	// CMS is the inventory-ledger integration. Empty base_url means the
	// subsystem does not run — configuration is the gate, there is no on/off
	// flag beside it that could disagree with the endpoint it is meant to
	// describe.
	CMS CMSConfig `yaml:"cms"`

	// Display holds the Phase 6 surfaces' numeric constants. Read it through
	// DisplayConstants(), not directly — see provenance.go, which also carries
	// the record of where each of these numbers came from and which of them a
	// plant has to re-derive.
	Display DisplayConfig `yaml:"display"`

	// Timezone is the plant's IANA zone for DISPLAY rendering (plant-local
	// timestamps on every core page, via shared/planttime) and for the
	// plant-local date-filter resolution (Q-004). Empty resolves to
	// America/Chicago — correct for both plants' wall clocks, and at
	// Hopkinsville it is the only thing making core right until the key is
	// seeded (the box's OS zone is Eastern; the plant clock is Central).
	// Storage and the wire stay UTC regardless; this field never touches
	// either. PLANT_TIMEZONE env still overrides it, so existing
	// deployments don't move on upgrade.
	Timezone string `yaml:"timezone"`
}

// DemandConfig tunes Core's reconciling sweep over demand episodes — the
// correctness floor under the six notification close paths.
//
// Every one of those paths is a notification: something happens, so something
// fires. SyncRegistry is the one that gets missed, because it is the only one
// where NOTHING fires — a binding can vanish and reappear in a single
// transaction with no RegistryChange emitted, and you cannot wire up an
// absence. The sweep closes any open episode whose precondition no longer
// holds regardless of how it stopped holding, which turns a missed close from
// "stranded forever" into "closed one sweep late".
//
// All three knobs are latency, not correctness: a longer interval means an
// ended demand keeps showing as open for longer, and nothing else.
type DemandConfig struct {
	// ReconcileInterval is the sweep cadence. Not a hot path — cost is bounded
	// by open-episode count, which is one per place currently short of
	// material — so a minute of latency on the rare miss costs nothing.
	// <= 0 disables the sweep entirely, which leaves the notification paths as
	// the only close mechanism; that is the pre-sweep behaviour and is
	// deliberately reachable for a plant that wants to bisect a problem.
	ReconcileInterval time.Duration `yaml:"reconcile_interval"`

	// ChildlessGrace is how long an episode may stay open with ZERO orders
	// against it before the sweep closes it `unattributed`.
	//
	// NOT OPTIONAL, and the reason is the deploy skew: a new Core against an
	// older Edge opens episodes whose orders come back with no origin on them,
	// so every open episode has zero children and — by the design's own display
	// rules, where a long-open episode is the loudest row on the page — the
	// whole surface reads as a plant-wide emergency that is really a deploy
	// artifact. Childless episodes are reachable at full parity too: Edge
	// silently drops threshold signals it cannot resolve.
	//
	// 15 minutes because a real demand that produces no order in that time is
	// itself the finding, and `unattributed` is how it gets said.
	ChildlessGrace time.Duration `yaml:"childless_grace"`

	// OrphanGrace is how long an order stamped `orphan` stays in the finding
	// set before the sweep ages it out.
	//
	// An orphan is an order that SHOULD have carried an origin and didn't, and
	// it is the only origin_class that is a finding. There is no deferred
	// attach — an orphan that later matches an open episode stays orphaned and
	// reconciles by a human — so without an expiry the finding set only ever
	// grows, and an alarm that never clears is indistinguishable from a broken
	// one. A day is long enough that a shift and a half can look at it.
	OrphanGrace time.Duration `yaml:"orphan_grace"`
}

// DispatchConfig tunes planner-side safety nets.
type DispatchConfig struct {
	Futility FutilityConfig `yaml:"futility"`
}

// FutilityConfig tunes the rate-per-tuple futility detector — the net for the
// class of failure where the planner turns a bounded physical condition into
// unbounded orchestration work (Springfield 2026-07-21: 484 doomed swaps in
// under two hours, none of which reached a robot, every surface green
// throughout).
//
// The threshold is ABSOLUTE and rate-based. A consecutive-run threshold is
// refuted by 120 days of plant history — normal operation produces runs of
// 5, 6, 8, 9 and one of 26, with no knee — while rate separates cleanly:
// ~4/h for the worst legitimate case against ~242/h for the cascade.
//
// A learned baseline is not on the table: 30 days of history spanning the
// incident would be trained on it, and the database has a 2.5-week hole
// (2026-06-27 → 07-15) that mis-baselines anything computed across it.
//
// THERE IS NO ENABLED FLAG, and its absence is the point. The detector RECORDS
// and does not act: one log line and one audit_log row, no chip, no alert, no
// brake. A switch on a thing that only observes buys nothing and costs the
// measurement — it shipped off-by-default and no plant ever opted in, so the
// one question anybody wants answered ("how often does a cell ask futilely,
// really?") has no data behind it at any site. Recording everywhere is what
// produces the number, and the number is what a threshold has to be set from.
//
// The THRESHOLD stays observe-only for the same reason it always was: a brake
// on an unmeasured threshold stops real work. Nothing here brakes until the
// records say where a real one belongs.
type FutilityConfig struct {
	// Threshold is how many futile terminals on one
	// (station, process_node, payload) inside Window trip the record.
	// Start at 20 — comfortably above the ~4/h worst legitimate case and far
	// below the cascade's ~242/h.
	Threshold int `yaml:"threshold"`
	// Window is the rolling window the count is taken over.
	Window time.Duration `yaml:"window"`
	// AlertThrottle suppresses repeat records for the same tuple. Modelled on
	// ThresholdMonitor's swapContradictionWindow, which is 15m for the same
	// reason: the condition persists, so the record should not repeat per-order.
	AlertThrottle time.Duration `yaml:"alert_throttle"`
}

// LoggingConfig gates what reaches stderr — under systemd, journald.
//
// debuglog mirrors every dbg() call to stderr. That mirror was unconditional
// until 2026-07-25, which put Springfield's journal at 633,129 lines/day and
// collapsed journald retention to ~15 days — shorter than the incidents being
// investigated. Over half of that volume was a 500ms poll loop that has since
// been retired outright, but the allow-list is what keeps the next one from
// costing the same.
//
// The ring buffer and the browser log UI are NOT gated by this. A muted
// subsystem is still fully readable in the UI; only the journal is quieter.
type LoggingConfig struct {
	// StderrSubsystems is the allow-list of debuglog subsystems mirrored to
	// stderr. Semantics:
	//
	//   absent          — the DefaultStderrSubsystems() list below
	//   ["all"]         — mirror everything (the incident escape hatch:
	//                     restore the full firehose without a rebuild)
	//   []  or  null    — mirror nothing; ring buffer and UI only
	//   ["a","b"]       — mirror exactly those
	//
	// Deliberately an allow-list, not a mute-list: a subsystem added later
	// stays out of the journal until someone opts it in, which is the
	// conservative direction for a resource that had already rotated away
	// the evidence this branch was written to preserve. Its absence is
	// visible — Core logs the effective list at boot, and the browser UI
	// shows every subsystem regardless.
	StderrSubsystems []string `yaml:"stderr_subsystems"`
}

// DefaultStderrSubsystems is the allow-list applied when logging config is
// absent: everything except the rds poll loop, which was 125,817 lines/day at
// Springfield and carries no signal that is not also in the ring buffer.
//
// Muting a subsystem's logging is NOT disabling it. The poller runs exactly as
// it did; only the journal is quieter, and the browser log UI still shows
// every subsystem.
func DefaultStderrSubsystems() []string {
	return []string{"dispatch", "engine", "core_handler", "kafka", "outbox", "protocol"}
}

// ResolveStderrSubsystems maps the YAML into debuglog.SetStderrSubsystems'
// argument: nil for "no restriction", otherwise the explicit allow-list.
func (l LoggingConfig) ResolveStderrSubsystems() []string {
	if slices.Contains(l.StderrSubsystems, "all") {
		return nil
	}
	if l.StderrSubsystems == nil {
		// Only reachable via an explicit `stderr_subsystems:` / `null` in the
		// YAML, since Defaults() prefills the field. Reads as "none".
		return []string{}
	}
	return l.StderrSubsystems
}

// ReplenishmentConfig tunes the UOP-threshold replenishment monitor (R1).
type ReplenishmentConfig struct {
	// LinesideDecisionMode selects which in-loop total the threshold monitor
	// decides replenishment firing off:
	//   "edge_reports" (default) — trust the Edge's per-consuming-node lineside
	//     reports: ledger total plus, for each FRESH reported node, (edge view −
	//     ledger view). A node whose report is missing OR stale (older than the
	//     monitor's staleness window) contributes no adjustment — its ledger term
	//     stands — and is flagged. This is R1 LIVE: it closes the SNF3 phantom-on-
	//     hand gap where a bin stranded `staged` keeps the ledger stocked while the
	//     line starves.
	//   "ledger" — decide off Core's ledger alone (the pre-R1 behavior). The revert
	//     knob: a plant reverts to pure-ledger by setting this, no code change.
	// Unknown values fall back to "edge_reports" with a warning. Either way the
	// ledger-vs-edge disagreement audit line is logged permanently.
	LinesideDecisionMode string `yaml:"lineside_decision_mode"`
}

type FireAlarmConfig struct {
	Enabled           bool `yaml:"enabled"`             // feature gate; false = hidden from UI
	AutoResumeDefault bool `yaml:"auto_resume_default"` // default checkbox state for auto-resume on clear
}

type NotificationsConfig struct {
	Enabled         bool     `yaml:"enabled"`
	SMTPHost        string   `yaml:"smtp_host"`
	SMTPPort        int      `yaml:"smtp_port"`
	SMTPTLS         bool     `yaml:"smtp_tls"`
	SMTPUser        string   `yaml:"smtp_user"`
	SMTPPassword    string   `yaml:"smtp_password"`
	FromAddress     string   `yaml:"from_address"`
	Recipients      []string `yaml:"recipients"`
	ThrottleMinutes int      `yaml:"throttle_minutes"`
}

// SimConfig configures the local-dev fleet simulator (core side). Sim code is
// behind //go:build sim AND requires SHINGO_ALLOW_SIM=1 at runtime; this struct
// only carries the knobs. See implementation-brief.md / docs/dev-env-api-gaps.md.
type SimConfig struct {
	Enabled     bool          `yaml:"enabled"`
	Seed        int64         `yaml:"seed"`         // PRNG seed; 0 = derive from time and log it
	Speed       float64       `yaml:"speed"`        // time multiplier: 2.0 = twice as fast. Default 1.0
	MaxSpeed    float64       `yaml:"max_speed"`    // effective-speed cap; <=0 → default (5×, the MEASURED ceiling — see clock.DefaultSimMaxSpeed). The integration sim can only process the real choreography so fast; past this the clock would outrun it and wedge, so requests are clamped here (honest readout shows asked-vs-running). Set very high to effectively uncap.
	Epoch       time.Time     `yaml:"epoch"`        // sim clock start (fast-forward origin). Zero = wall-now
	AnchorWall  time.Time     `yaml:"anchor_wall"`  // SHARED wall anchor for fast-forward sync: sim-now = epoch + speed×(wallNow−anchor). Set IDENTICALLY in core+edge to the run-start wall time so the two clocks stay in lockstep (no cross-process drift). Zero = per-process boot anchor (drifts — only safe single-process).
	TransitTime time.Duration `yaml:"transit_time"` // base per-block transit; default 5s
	JitterPct   float64       `yaml:"jitter_pct"`   // ± fraction applied to transit; default 0.2
	FailRate    float64       `yaml:"fail_rate"`    // 0.0–1.0 per-transition fault probability; default 0

	// Finite-fleet model (G16). Defaults preserve the legacy infinite-fleet
	// behaviour (one synthetic robot per active order, flat transit), so a
	// config that sets none of these runs exactly as before.
	FleetSize  int           `yaml:"fleet_size"`  // 0 = infinite fleet (default); >0 = finite robot pool, orders queue for a free robot
	TransitMin time.Duration `yaml:"transit_min"` // min per-move transit; 0 falls back to transit_time ± jitter
	TransitMax time.Duration `yaml:"transit_max"` // max per-move transit (uniform draw with transit_min); must exceed transit_min to take effect
}

// Scaled divides a duration by the speed multiplier (G4). Zero or negative
// speed is treated as 1.0 (no scaling).
func (s SimConfig) Scaled(d time.Duration) time.Duration {
	if s.Speed <= 0 {
		return d
	}
	return time.Duration(float64(d) / s.Speed)
}

// SourceabilityConfig tunes the plant-wide sourceability computation — the
// always-on read that tells every process which styles it can change over to.
type SourceabilityConfig struct {
	// EnableAtRisk lets a satisfiable-but-projected-empty style report YELLOW.
	// Default false: the plant sees green/red only until the owner validates the
	// consumption-rate window on real audit data, then flips this on. The at-risk
	// tier is always COMPUTED; this only controls whether it surfaces as a status.
	EnableAtRisk bool `yaml:"enable_at_risk"`
	// RateWindow is the look-back for the per-payload consumption rate that feeds
	// time-to-empty. Default 30m.
	RateWindow time.Duration `yaml:"rate_window"`
	// Horizon: a line projecting empty within this window is at risk. Default 30m.
	Horizon time.Duration `yaml:"horizon"`
}

type StagingConfig struct {
	// TTL is the global default staging expiry. 0 (the default) means permanent:
	// staged bins never auto-unstage — they're released only by the next claim
	// or by operator action. Override per-node via the `staging_ttl` property
	// (admin UI) on a specific node or its parent.
	TTL                  time.Duration `yaml:"ttl"`                    // default 0 (permanent)
	SweepInterval        time.Duration `yaml:"sweep_interval"`         // default 5m
	AutoConfirmDelivered time.Duration `yaml:"auto_confirm_delivered"` // 0 = disabled
	// AbandonStuck cancels orders stuck past this age. It covers exactly TWO
	// statuses — dispatched and staged — the ones where the fleet has the order
	// and nothing is moving: a leg handed over that never started (the
	// long-weekend drain case), or a robot parked at a staging node.
	// protocol.IsStuckSweepCandidate is the authority; this comment is not.
	//
	// It does NOT cover queued or sourcing, and that is deliberate: demand is
	// operator-driven and does not evaporate, so a waiting order holds
	// indefinitely rather than being cancelled on a timer. in_transit is
	// excluded too — a robot that is actually moving is not stuck.
	//
	// This comment used to list queued and sourcing as covered, describing the
	// wider set the sweep had before it was narrowed. Worth knowing what that
	// cost a reader: queued was not swept AND was outside
	// protocol.IsRuntimeStuckCandidate, so a wedged queued order raised no
	// anomaly either. It was the least observable state in the system, and this
	// comment said it was covered.
	//
	// Half of that is now fixed: queued joined IsRuntimeStuckCandidate on
	// 2026-08-03, so a wedged one raises a `degraded` anomaly after 30 minutes.
	// It is still deliberately NOT swept, for the reason two paragraphs up —
	// flagging it for a person and cancelling it on a timer are different
	// answers, and only the first one is right for demand that has not gone away.
	//
	// Cascades to the two-robot sibling.
	AbandonStuck time.Duration `yaml:"abandon_stuck"` // default 1h; 0 = disabled
	// AbandonStuckOperatorGated is the SEPARATE, longer bound for a staged leg
	// whose release is a HUMAN action rather than a system step — a coordinated
	// two-robot swap parked at its wait point (dispatch.IsOperatorGatedStaging).
	//
	// AbandonStuck's premise is "a robot parked this long has been forgotten".
	// That premise is wrong for a pair an operator still has to authorise, and
	// at Springfield on 2026-07-31 it destroyed both legs of a live changeover
	// at exactly 1h: the evac staged at 15:00, its supply sibling arrived 15:32
	// after three transient fleet faults, and the sweep cancelled the evac at
	// 16:00 and cascaded the supply.
	//
	// Still BOUNDED rather than exempt, so a genuinely forgotten swap cannot
	// park two robots forever. 0 = never auto-cancel an operator-gated leg.
	AbandonStuckOperatorGated time.Duration `yaml:"abandon_stuck_operator_gated"` // default 4h; 0 = never
}

type DatabaseConfig struct {
	Postgres PostgresConfig `yaml:"postgres"`
}

type PostgresConfig struct {
	Host            string        `yaml:"host"`
	Port            int           `yaml:"port"`
	Database        string        `yaml:"database"`
	User            string        `yaml:"user"`
	Password        string        `yaml:"password"`
	SSLMode         string        `yaml:"sslmode"`
	MaxOpenConns    int           `yaml:"max_open_conns"`
	MaxIdleConns    int           `yaml:"max_idle_conns"`
	ConnMaxLifetime time.Duration `yaml:"conn_max_lifetime"`
}

type RDSConfig struct {
	BaseURL      string        `yaml:"base_url"`
	PollInterval time.Duration `yaml:"poll_interval"`
	Timeout      time.Duration `yaml:"timeout"`
	// FaultGrace is how long an order sits in `faulted` after RDS reports
	// FAILED before Core gives up and fails it. A robot that recovers
	// inside the window (FAILED->RUNNING) clears the deadline and the
	// order carries on, so this is really "how long we let the floor sort
	// a stuck AMR out before the order is written off".
	//
	// It is the OUTER bound. FaultNoticeAfter below is the inner one, and the
	// two together are the whole fault policy: nothing is said before the
	// inner, everything is over at the outer.
	FaultGrace time.Duration `yaml:"fault_grace"`
	// FaultNoticeAfter is how long an order must have been faulted before the
	// floor is told the word "fault". Below it the order is described as
	// REPLANNING; at or above it, as a FAULT with the fleet's reason.
	//
	// THIS IS THE ONLY NUMBER THAT CHANGES WHAT AN OPERATOR IS TOLD, which is
	// why it is config and not a constant in five JS files. 30 days of
	// Springfield history: 730 faulted transitions, 706 of them recovered on
	// their own with a median of 20 seconds. A badge that fires on all 730
	// trains the floor to ignore the 24 that matter. Sixty seconds is three
	// times that median and well inside any grace window — it hides the
	// replans and shows the stalls.
	//
	// It is a default, not a truth. A plant whose robots re-plan more slowly
	// will hide real stalls at 60s, and one with a faster fleet will cry fault
	// at noise; both are a config edit, which is the point.
	//
	// Must be greater than zero and strictly less than FaultGrace — see
	// RDSConfig.Validate. At or above the grace window it could never fire,
	// because the order is failed by then.
	FaultNoticeAfter time.Duration `yaml:"fault_notice_after"`
	// StrandedSweepWindow is how recently a `_TRANSIT` bin's order must have
	// ended for the reconciliation sweep to infer where that bin was set down.
	//
	// ONE NUMBER, THREE INTERVALS, and they are three readings of the same
	// sentence — "telemetry stops describing a bin this long after the fact":
	//
	//	1  the terminal window: how long after the order ended the sweep will
	//	   still run the inference at all (engine.terminalWithin);
	//	2  the pickup window: how long after the bin LEFT THE FLOOR branch A
	//	   will believe the robot's current position (engine.pickupWithin) —
	//	   the terminal row cannot bound this, because on the event path it is
	//	   milliseconds old by construction;
	//	3  the observation window: how long a FROZEN drop reading stays worth
	//	   acting on when the placement it wants keeps being refused
	//	   (engine.freezeDrop).
	//
	// THE SWEEP IS A BACKSTOP FOR THE FAST PATH, NOT A BACKFILL OF HISTORY, and
	// this number is what makes that true. The inference reads the robot's
	// CURRENT telemetry: where it is standing now, and whether its deck is
	// empty now. That answers "where did this bin go" only while the robot has
	// not moved on. An hour later the robot has run a dozen other jobs and its
	// position says nothing about a bin it set down before them — placing on it
	// would invent a bin at a node the floor would then be sent to fetch.
	//
	// So bins older than this window are LEFT AS ANOMALIES for an operator to
	// resolve, which is what they were before any of this shipped. Nothing is
	// lost by declining; something real is broken by guessing.
	//
	// Two hours is comfortably longer than FaultGrace (45m default), so an
	// order that faulted, sat out its whole grace window and was written off is
	// still inside it — that is the case the sweep exists to catch when the
	// terminal-event hook is missed.
	//
	// THE JACK IS STILL THE JACK for a carrier (`_ROBOT:*`) bin: how long that
	// bin has been RIDING is not gated, and a deck that reports empty after a
	// week has still set its bin down, so the freeze forms on that tick and the
	// placement follows from it. What reading 3 bounds is the age of the
	// OBSERVATION afterwards — a drop that was watched but could not be placed
	// (an occupied slot, a point the scene cannot name) stops being safe to act
	// on once enough time has passed for somebody to have moved the bin by hand.
	// Past that the watch declines and says how old the reading is; it does not
	// take a fresh one, because by then the robot is standing somewhere else
	// entirely and a fresh reading would place the bin there.
	StrandedSweepWindow time.Duration `yaml:"stranded_sweep_window"`
}

// Validate reports a fault-window configuration that cannot do its job.
//
// Reported, not fatal: the caller (Load) falls back to the shipped default for
// the offending field rather than refusing to boot. A number that decides what
// wording an operator sees must not be able to stop a plant's core from
// starting — the same reasoning as DisplayConfig.Validate, and the same
// reasoning as the zero-guard on the fault_grace form field
// (handlers_config.go). A plant that lowers fault_grace below the notice
// threshold gets a working core and a log line, not a dead one.
func (r RDSConfig) Validate() error {
	if r.FaultNoticeAfter <= 0 {
		return fmt.Errorf("rds: fault_notice_after (%s) must be greater than zero — "+
			"at zero every replan is announced as a fault", r.FaultNoticeAfter)
	}
	if r.FaultGrace > 0 && r.FaultNoticeAfter >= r.FaultGrace {
		return fmt.Errorf("rds: fault_notice_after (%s) must be strictly less than fault_grace (%s) — "+
			"at or above it the notice could never fire, because the order is failed by then",
			r.FaultNoticeAfter, r.FaultGrace)
	}
	return nil
}

type WebConfig struct {
	Host          string `yaml:"host"`
	Port          int    `yaml:"port"`
	SessionSecret string `yaml:"session_secret"`
}

type MessagingConfig struct {
	Kafka               KafkaConfig   `yaml:"kafka"`
	OrdersTopic         string        `yaml:"orders_topic"`
	DispatchTopic       string        `yaml:"dispatch_topic"`
	OutboxDrainInterval time.Duration `yaml:"outbox_drain_interval"`
	StationID           string        `yaml:"station_id"`
	SigningKey          string        `yaml:"signing_key"` // optional HMAC-SHA256 shared secret for envelope signing
	// StaleEdgeThreshold is how long an edge can go without a heartbeat
	// before core marks it stale and reaps its demand_registry rows.
	// Zero falls back to the 15 minute default. Tune down for faster
	// reaction to edge failures at the cost of more false positives on
	// flaky links; tune up if edges routinely pause longer than 15 min.
	StaleEdgeThreshold time.Duration `yaml:"stale_edge_threshold"`
}

type KafkaConfig struct {
	Brokers []string `yaml:"brokers"`
	GroupID string   `yaml:"group_id"`
	// DialTimeout bounds the per-broker reachability probe in Connect.
	// Zero means the 5s production default. A config that names unreachable
	// brokers pays this per broker, serially — which is why it is a knob at
	// all: the config-save handler reconfigures messaging inline, and a test
	// (or a plant) saving broker names that don't resolve would hold the
	// handler for 5s × brokers.
	DialTimeout time.Duration `yaml:"dial_timeout"`
}

// DialTimeoutOr returns the effective broker-probe timeout: the configured
// value, or the 5s production default when zero.
func (k KafkaConfig) DialTimeoutOr() time.Duration {
	if k.DialTimeout > 0 {
		return k.DialTimeout
	}
	return 5 * time.Second
}

// RobotConfidenceConfig tunes the localization-confidence collector, which
// samples SEER's rbk_report.confidence off Core's existing 2-second robot
// poll. It adds no load on RDS — it taps a poll that already runs.
//
// Nothing anywhere retains a history of this figure: there is no %confid%
// column in the RDS MariaDB and no history endpoint in the RDS HTTP API. So
// there is nothing to backfill, and every day not collected is lost. The
// shape of the data is fixed by migration v77; these are the knobs that
// decide how much of it is kept.
//
// ONE OF THESE IS REVERSIBLE AND THE REST OF THE DESIGN IS NOT. Retention is
// a dial: daily partitions make changing it a config edit plus the next run
// of the drop loop, with nothing rewritten and nothing recomputed. Start at
// the default, measure a day of real traffic, then set it. If volume ever
// forces a cut, CUT DAYS — never sample. Down-sampling the healthy readings
// looks tempting and would cut volume by more than half, but it silently
// breaks p05 and the location baseline: fewer days gives correct numbers over
// a shorter period, while sampling gives wrong numbers over a longer one.
type RobotConfidenceConfig struct {
	// Enabled false skips the write path ENTIRELY rather than writing and
	// discarding — the kill switch if a plant sees any load surprise.
	Enabled bool `yaml:"enabled"`

	// RawRetentionDays is how long full-resolution samples are kept. Two
	// weeks is the shortest window that can answer "is this new?" from raw
	// data; seven days gives one week and nothing to compare it against.
	RawRetentionDays int `yaml:"raw_retention_days"`

	// LowConfidenceThreshold is both the double-write cut for the forensic
	// trail and clause 3 of the write rule.
	LowConfidenceThreshold float64 `yaml:"low_confidence_threshold"`

	// LowConfidenceRetentionDays keeps the low trail far longer than raw:
	// the row count is tiny and it is what an incident review reads.
	LowConfidenceRetentionDays int `yaml:"low_confidence_retention_days"`

	// DeadBandMetres and DeadBandConfidence are clauses 1 and 2 of the write
	// rule — how far a robot must move, or how much the number must change,
	// since the last STORED sample.
	DeadBandMetres     float64 `yaml:"dead_band_metres"`
	DeadBandConfidence float64 `yaml:"dead_band_confidence"`

	// The three rate limits on the clauses that fire while a robot is
	// stationary. Without them a robot sitting in a bad state would store a
	// row every poll.
	LowInterval    time.Duration `yaml:"low_interval"`    // clause 3
	StuckInterval  time.Duration `yaml:"stuck_interval"`  // clause 4
	FailedInterval time.Duration `yaml:"failed_interval"` // clause 5

	// SnapToleranceMetres is how far a sample may sit from a path segment and
	// still be attributed to it by the nightly roll-up. Generous by
	// necessity: scene_edges stores only segment endpoints, so a curved path
	// is snapped against its chord, which at Springfield diverges from the
	// driven lane by up to 1.30 m. Read-time only — changing it re-bins
	// future roll-ups and never touches a stored sample.
	SnapToleranceMetres float64 `yaml:"snap_tolerance_metres"`

	// BaselineDays is the trailing window the per-segment fleet median is
	// computed over. It must not be same-day: against a same-day baseline a
	// plant-wide degradation moves the median with it and the event vanishes.
	BaselineDays int `yaml:"baseline_days"`
}

func Defaults() *Config {
	return &Config{
		Database: DatabaseConfig{
			Postgres: PostgresConfig{
				Host:     "localhost",
				Port:     5432,
				Database: "shingocore",
				User:     "shingocore",
				Password: "",
				SSLMode:  "disable",
			},
		},
		RDS: RDSConfig{
			BaseURL:             "http://192.168.1.100:8088",
			PollInterval:        5 * time.Second,
			Timeout:             10 * time.Second,
			FaultGrace:          45 * time.Minute,
			FaultNoticeAfter:    defaultFaultNoticeAfter,
			StrandedSweepWindow: defaultStrandedSweepWindow,
		},
		Web: WebConfig{
			Host:          "0.0.0.0",
			Port:          8083,
			SessionSecret: "change-me-in-production",
		},
		Staging: StagingConfig{
			TTL:                  0, // 0 = never auto-unstage; override per node group via staging_ttl property
			SweepInterval:        5 * time.Minute,
			AutoConfirmDelivered: 5 * time.Minute, // auto-confirm delivered orders after 5 minutes if no receipt from Edge
			AbandonStuck:         time.Hour,       // cancel orders stuck queued/staged for 1h (ties up robots, clutters the board)
			// Operator-gated staging gets 4h, not 1h: the wait is a human
			// decision, and 1h is shorter than a changeover legitimately runs.
			AbandonStuckOperatorGated: 4 * time.Hour,
		},
		Sourceability: SourceabilityConfig{
			EnableAtRisk: false, // green/red only until the owner validates the rate window on plant data
			RateWindow:   30 * time.Minute,
			Horizon:      30 * time.Minute,
		},
		Logging: LoggingConfig{
			StderrSubsystems: DefaultStderrSubsystems(),
		},
		Dispatch: DispatchConfig{
			Futility: FutilityConfig{
				Threshold:     20,
				Window:        60 * time.Minute,
				AlertThrottle: 15 * time.Minute,
			},
		},
		Replenishment: ReplenishmentConfig{
			// R1 LIVE by default: decide off the Edge lineside reports (ledger +
			// fresh-node adjustments). Set "ledger" to revert to pure-ledger.
			LinesideDecisionMode: "edge_reports",
		},
		Demand: DemandConfig{
			// ON by default, unlike the futility detector. That detector CREATES
			// records a plant has to interpret; this one only ends episodes that
			// have already ended, and shipping it opt-out would mean the
			// correctness floor is absent exactly at the plants that never
			// edited a YAML.
			ReconcileInterval: 60 * time.Second,
			ChildlessGrace:    15 * time.Minute,
			OrphanGrace:       24 * time.Hour,
		},
		Messaging: MessagingConfig{
			Kafka: KafkaConfig{
				Brokers: []string{"localhost:9092"},
				GroupID: "shingocore",
			},
			OrdersTopic:         "shingo.orders",
			DispatchTopic:       "shingo.dispatch",
			OutboxDrainInterval: 5 * time.Second,
			StationID:           "core",
			StaleEdgeThreshold:  15 * time.Minute,
		},
		Notifications: NotificationsConfig{
			Enabled:         false,
			SMTPHost:        "localhost",
			SMTPPort:        587,
			SMTPTLS:         true,
			ThrottleMinutes: 15,
		},
		Sim: SimConfig{
			// Enabled false by default; Seed 0 = derive+log. Sane sim timings so a
			// dev YAML can flip enabled:true without specifying every knob.
			TransitTime: 5 * time.Second,
			JitterPct:   0.2,
		},
		// ON by default. The collector taps a poll Core already makes, so it
		// adds no vendor load, and the data it gathers cannot be recovered
		// later — shipping it opt-out would mean the plants that never edit a
		// YAML are exactly the ones with no history when someone finally asks
		// why a robot keeps stranding in one aisle.
		RobotConfidence: RobotConfidenceConfig{
			Enabled:                    true,
			RawRetentionDays:           14,
			LowConfidenceThreshold:     0.50,
			LowConfidenceRetentionDays: 90,
			DeadBandMetres:             0.25,
			DeadBandConfidence:         0.02,
			LowInterval:                10 * time.Second,
			StuckInterval:              30 * time.Second,
			FailedInterval:             10 * time.Second,
			SnapToleranceMetres:        2.0,
			BaselineDays:               14,
		},
		// Values and the reasoning behind each of them live in provenance.go,
		// together, so that neither can be edited without the other in view.
		Display: DisplayDefaults(),
		CMS:     CMSDefaults(),

		// Empty, not "America/Chicago": the www layer owns the default and
		// logs which source resolved, so a plant running on the default is
		// told so in the journal rather than the value hiding here.
		Timezone: "",
	}
}

func Load(path string) (*Config, error) {
	cfg := Defaults()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, err
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	// A bad fault window degrades to a working one rather than killing the boot.
	// See RDSConfig.Validate for why this is reported and not fatal.
	if err := cfg.RDS.Validate(); err != nil {
		fallback := defaultFaultNoticeAfter
		if cfg.RDS.FaultGrace > 0 && fallback >= cfg.RDS.FaultGrace {
			// A grace window shorter than the default notice. The notice still
			// has to fit inside it or it can never fire, so it takes half the
			// window — the replan/fault distinction survives at any grace.
			fallback = cfg.RDS.FaultGrace / 2
		}
		log.Printf("config: %v — using fault_notice_after=%s", err, fallback)
		cfg.RDS.FaultNoticeAfter = fallback
	}
	// A zero window is what every pre-2026-08-22 config file carries, and it
	// would read as "sweep nothing" — the inference would decline every bin and
	// the backstop would silently stop being one. Own that here rather than
	// letting an absent key turn a feature off. Not part of RDSConfig.Validate:
	// that reports ONE error and Load's fallback above repairs the fault window,
	// so a second condition sharing it would misassign the repair.
	if cfg.RDS.StrandedSweepWindow <= 0 {
		cfg.RDS.StrandedSweepWindow = defaultStrandedSweepWindow
	}
	// FATAL, unlike the RDS validation ten lines up, and the difference is
	// deliberate. That one repairs a number deciding what wording an operator
	// sees; this one decides whether an inventory ledger receives what shingo
	// believes about the plant's stock. A core that boots with a CMS URL and no
	// keys posts nothing, banks the failures in a table nobody is watching, and
	// reports itself healthy. The refusal has to land at boot, where somebody
	// is looking.
	if err := cfg.CMS.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) Save(path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func (c *Config) Lock()   { c.mu.Lock() }
func (c *Config) Unlock() { c.mu.Unlock() }

// TryLock attempts to acquire the write lock without blocking, reporting
// whether it succeeded. Companion to Lock/Unlock; lets callers assert the lock
// is free without risking a hang on a deadlock.
func (c *Config) TryLock() bool { return c.mu.TryLock() }

// CMSConfig is the middleware inventory-ledger integration.
//
// CONFIGURATION IS THE GATE. An empty BaseURL means the subsystem does not
// run — there is no separate enable flag, because a flag and an endpoint can
// disagree and then two things have to be right for one behaviour. Springfield
// carries no cms: block and therefore no CMS activity, without anyone having
// to remember to turn it off.
type CMSConfig struct {
	// BaseURL is the middleware's inventory_transactions endpoint. Empty
	// disables the whole subsystem.
	BaseURL string `yaml:"base_url"`
	// AccessKey and SecretKey go in the site-local yaml, never the repo.
	// They are never logged, never in an error, and String() renders them as
	// <set>/<empty>.
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`

	Timeout      time.Duration `yaml:"timeout"`
	PollInterval time.Duration `yaml:"poll_interval"`
	MaxAttempts  int           `yaml:"max_attempts"`
	// SettleWindow is how long an inflight posting is left alone before the
	// reconciler asks the middleware about it. A POST that succeeded may not
	// be queryable for a moment afterwards, and asking too early gets "no such
	// transaction" about something that is about to exist — an answer that
	// would requeue a posting that already landed.
	SettleWindow time.Duration `yaml:"settle_window"`
	// MaxRequeues bounds how many times the reconciler may return one posting
	// to the send queue before giving up on it.
	//
	// Separate from MaxAttempts because it counts a different event. An attempt
	// is a send that failed; a requeue is the middleware telling us it does NOT
	// hold a transaction we thought it might, which puts the row back in the
	// queue with its attempt budget intact. That is the right behaviour for one
	// bad round trip and a loop if the same 5xx repeats — and until this
	// existed the loop had no ceiling at all, because nothing read the
	// requeue_count the reconciler was incrementing.
	MaxRequeues int `yaml:"max_requeues"`
	// HealthWindow is how far back a REJECTED or FAILED posting still counts
	// toward the diagnostics verdict.
	//
	// It exists because those two statuses are permanent marks on a row: one
	// refusal, ever, held the verdict at "attention" forever with no way back
	// to green and no acknowledge path. A health surface that cannot return to
	// green stops being read, which costs more than the silence it was built
	// to prevent. Rows outside the window are still counted on the page and
	// still named in the healthy sentence — they stop being the VERDICT, not
	// stop existing.
	HealthWindow time.Duration `yaml:"health_window"`

	// The vocabulary CMS expects. Placeholders until SCO confirms them; every
	// one is a config edit rather than a code change for exactly that reason —
	// including department and operation, which were hardcoded "" in the
	// translator and would otherwise have been the two exceptions to that
	// promise.
	ReasonCode    string `yaml:"reason_code"`
	IncreaseType  string `yaml:"increase_type"`
	DecreaseType  string `yaml:"decrease_type"`
	UnitOfMeasure string `yaml:"unit_of_measure"`
	UserID        string `yaml:"user_id"`
	// Blank in the vendor's sample. Declared so a value is a yaml edit.
	Department string `yaml:"department"`
	Operation  string `yaml:"operation"`

	// Bin is the CMS bin code this site's rows carry — master data CMS owns,
	// not anything shingo can derive. See wire.Config.Bin. Empty ships an empty
	// Bin, which the middleware refuses; no site in this repository sets one.
	Bin string `yaml:"bin"`

	// InsecureSkipVerify disables TLS certificate verification on the calls to
	// the middleware. It defaults to FALSE and the repository ships no site
	// that sets it — it is opt-in, per site, in the site-local yaml.
	//
	// It exists because Hopkinsville's middleware presents a certificate issued
	// by `Martinrea International Root CA`, an internal CA that is pushed to
	// domain-joined Windows machines by group policy and is therefore absent
	// from a Linux core box's trust store. The endpoint owner's instruction
	// (2026-09-08) was to skip verification rather than distribute the root.
	//
	// WHAT IT COSTS, so that whoever reads this next is not deciding blind:
	// verification is the only thing establishing that the host answering is
	// the middleware. With it off — and with `mes-apps.martinrea.com` pinned to
	// an IP in /etc/hosts, which is how that box resolves it — nothing
	// authenticates the far end, and every request carries access_key and
	// secret_key in its headers. The alternative that keeps verification is one
	// file in /usr/local/share/ca-certificates/ and `update-ca-certificates`.
	//
	// It is a per-site field rather than a constant in the client for exactly
	// one reason: a site that never sets it must not inherit this. Springfield
	// carries no cms: block at all today, and when it gets one it should start
	// from a verifying default and have to choose otherwise in writing.
	InsecureSkipVerify bool `yaml:"insecure_skip_verify"`
}

// DefaultCMSHealthWindow lives here rather than in a subsystem because no
// subsystem owns it: the window is a property of the diagnostics verdict, which
// is assembled in service/ from config. See CMSConfig.HealthWindow.
const DefaultCMSHealthWindow = 24 * time.Hour

// CMSDefaults returns the shipped CMS configuration: disabled, with the
// vocabulary and timings pre-filled so a site turns the integration on by
// supplying a URL and two keys.
func CMSDefaults() CMSConfig {
	return CMSConfig{
		BaseURL:       "",
		Timeout:       client.DefaultTimeout,
		PollInterval:  poster.DefaultPollInterval,
		MaxAttempts:   poster.DefaultMaxAttempts,
		SettleWindow:  poster.DefaultSettleWindow,
		MaxRequeues:   poster.DefaultMaxRequeues,
		HealthWindow:  DefaultCMSHealthWindow,
		ReasonCode:    "TEST-AMR",
		IncreaseType:  "I",
		DecreaseType:  "D",
		UnitOfMeasure: "EA",
		UserID:        "SHINGO",
	}
}

// Enabled reports whether the CMS subsystem should run.
func (c CMSConfig) Enabled() bool { return c.BaseURL != "" }

// Client, Poster and Wire map this block onto the three structs the subsystem
// actually takes.
//
// THE MAPPING LIVES BESIDE THE FIELDS IT MAPS. It was twelve lines of manual
// copying in engine_lifecycle.go, which is the shape where a field added here
// is silently not carried: nothing fails to compile, the subsystem just runs on
// a zero value. TestCMSConfig_MappersCarryEveryField walks the three
// destination structs by reflection and fails on any field left at its zero
// value, so adding one here without mapping it is a test failure rather than a
// production default nobody chose.
func (c CMSConfig) Client() client.Config {
	return client.Config{
		BaseURL:            c.BaseURL,
		AccessKey:          c.AccessKey,
		SecretKey:          c.SecretKey,
		Timeout:            c.Timeout,
		InsecureSkipVerify: c.InsecureSkipVerify,
	}
}

func (c CMSConfig) Poster() poster.Config {
	return poster.Config{
		PollInterval: c.PollInterval,
		MaxAttempts:  c.MaxAttempts,
		SettleWindow: c.SettleWindow,
		MaxRequeues:  c.MaxRequeues,
		Wire:         c.Wire(),
	}
}

func (c CMSConfig) Wire() wire.Config {
	return wire.Config{
		ReasonCode:    c.ReasonCode,
		IncreaseType:  c.IncreaseType,
		DecreaseType:  c.DecreaseType,
		UnitOfMeasure: c.UnitOfMeasure,
		UserID:        c.UserID,
		Department:    c.Department,
		Operation:     c.Operation,
		Bin:           c.Bin,
	}
}

// Validate refuses a configuration that would start the integration without
// the credentials to use it, and fills zero durations from the defaults.
//
// FATAL, AND DELIBERATELY UNLIKE RDSConfig.Validate, which is reported and not
// fatal. That distinction is the point rather than an inconsistency: RDS's
// validated field decides what wording an operator sees, and a bad number
// there must not stop a plant's core from starting. This one decides whether
// an inventory ledger receives what shingo believes about the plant's stock.
// A core that starts with a URL and no keys posts nothing, records the
// failures in a table nobody is watching, and looks healthy — so the failure
// has to happen at boot, where somebody is.
//
// An empty BaseURL is not an error. It is how a site says it does not use CMS,
// and the rest of the block is then irrelevant.
func (c *CMSConfig) Validate() error {
	// Every fallback here reads the SAME constant CMSDefaults does, and the
	// subsystems' own zero-value fallbacks read it too. A default spelled in
	// more than one place is a value somebody retunes in one of them.
	if c.Timeout <= 0 {
		c.Timeout = client.DefaultTimeout
	}
	if c.PollInterval <= 0 {
		c.PollInterval = poster.DefaultPollInterval
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = poster.DefaultMaxAttempts
	}
	if c.SettleWindow <= 0 {
		c.SettleWindow = poster.DefaultSettleWindow
	}
	if c.MaxRequeues <= 0 {
		c.MaxRequeues = poster.DefaultMaxRequeues
	}
	if c.HealthWindow <= 0 {
		c.HealthWindow = DefaultCMSHealthWindow
	}
	if !c.Enabled() {
		return nil
	}
	// PARSED HERE, WHERE THE FAILURE IS CHEAP. A base_url that will not parse
	// booted fine and then failed inside every POST, one row at a time, in a
	// table nobody is watching — for a typo one yaml edit repairs. Requiring a
	// scheme and a host rather than merely "url.Parse returned no error" is the
	// point: url.Parse accepts almost anything, and "middleware.example.com"
	// with the https:// forgotten parses cleanly as a relative path.
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return fmt.Errorf("cms: base_url %q does not parse: %w", c.BaseURL, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("cms: base_url %q needs a scheme and a host (e.g. "+
			"https://middleware.example.com/api/inventory_transactions)", c.BaseURL)
	}
	if c.AccessKey == "" || c.SecretKey == "" {
		return fmt.Errorf("cms: base_url is set (%s) but access_key or secret_key is empty — "+
			"the integration would accept every movement and post none of them", c.BaseURL)
	}
	return nil
}

// String renders the config with the keys redacted.
//
// It exists so that a `%v` or `%+v` of a Config — a debug print, a panic dump,
// a log line written in a hurry — cannot spill the credentials. Go picks this
// up for both verbs because it satisfies fmt.Stringer.
func (c CMSConfig) String() string {
	return fmt.Sprintf("CMSConfig{base_url:%s access_key:%s secret_key:%s timeout:%s "+
		"poll_interval:%s max_attempts:%d settle_window:%s max_requeues:%d health_window:%s "+
		"reason_code:%s increase_type:%s decrease_type:%s unit_of_measure:%s user_id:%s "+
		"department:%s operation:%s bin:%s insecure_skip_verify:%t}",
		c.BaseURL, redacted(c.AccessKey), redacted(c.SecretKey), c.Timeout,
		c.PollInterval, c.MaxAttempts, c.SettleWindow, c.MaxRequeues, c.HealthWindow,
		c.ReasonCode, c.IncreaseType, c.DecreaseType, c.UnitOfMeasure, c.UserID,
		c.Department, c.Operation, c.Bin, c.InsecureSkipVerify)
}

// redacted reports whether a secret is present without saying what it is. It
// says <set> rather than a length or a prefix: both leak, and a prefix leaks
// the part an attacker would guess from.
func redacted(s string) string {
	if s == "" {
		return "<empty>"
	}
	return "<set>"
}
