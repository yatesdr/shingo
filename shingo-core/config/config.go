package config

import (
	"os"
	"slices"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	mu sync.RWMutex `yaml:"-"`

	Database      DatabaseConfig      `yaml:"database"`
	RDS           RDSConfig           `yaml:"rds"`
	Web           WebConfig           `yaml:"web"`
	Messaging     MessagingConfig     `yaml:"messaging"`
	Staging       StagingConfig       `yaml:"staging"`
	CountGroups   CountGroupsConfig   `yaml:"count_groups"`
	FireAlarm     FireAlarmConfig     `yaml:"fire_alarm"`
	Notifications NotificationsConfig `yaml:"notifications"`
	Sim           SimConfig           `yaml:"sim"`
	Sourceability SourceabilityConfig `yaml:"sourceability"`
	Replenishment ReplenishmentConfig `yaml:"replenishment"`
	Logging       LoggingConfig       `yaml:"logging"`
	Dispatch      DispatchConfig      `yaml:"dispatch"`
	Demand        DemandConfig        `yaml:"demand"`

	RobotConfidence RobotConfidenceConfig `yaml:"robot_confidence"`

	// Display holds the Phase 6 surfaces' numeric constants. Read it through
	// DisplayConstants(), not directly — see provenance.go, which also carries
	// the record of where each of these numbers came from and which of them a
	// plant has to re-derive.
	Display DisplayConfig `yaml:"display"`
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
type FutilityConfig struct {
	// Enabled gates the whole detector. Off by default: it ships observe-only
	// and a plant opts in.
	Enabled bool `yaml:"enabled"`
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
// until 2026-07-25, which put Springfield's journal at 633,129 lines/day (53%
// of it the two countgroup poll lines at a 500ms tick) and collapsed journald
// retention to ~15 days — shorter than the incidents being investigated.
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
// absent: everything except the two poll loops. countgroup (334,361 lines/day)
// and rds (125,817) are 75% of the journal between them and neither carries a
// signal that is not also in the ring buffer.
//
// Muting countgroup's logging is NOT disabling countgroup. The interlock
// returns 1-2 robots 6,265 times a day and stays enabled; see rds/robots.go.
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

// CountGroupsConfig configures the advanced-zone polling feature.
// Empty Groups slice ⇒ feature disabled.
// All fields are overridable per-deployment via shingocore.yaml.
type CountGroupsConfig struct {
	PollInterval       time.Duration      `yaml:"poll_interval"`
	RDSTimeout         time.Duration      `yaml:"rds_timeout"`
	OnThreshold        int                `yaml:"on_threshold"`
	OffThreshold       int                `yaml:"off_threshold"`
	FailSafeTimeout    time.Duration      `yaml:"fail_safe_timeout"`
	NeverOccupiedWarn  time.Duration      `yaml:"never_occupied_warn"`
	NeverOccupiedError time.Duration      `yaml:"never_occupied_error"`
	Groups             []CountGroupConfig `yaml:"groups"`
}

type CountGroupConfig struct {
	Name    string `yaml:"name"`
	Enabled bool   `yaml:"enabled"`
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
	MaxSpeed    float64       `yaml:"max_speed"`    // effective-speed cap; <=0 → default (15×). The integration sim can only process the real choreography so fast; past this the clock would outrun it and wedge, so requests are clamped here (honest readout shows asked-vs-running). Set very high to effectively uncap.
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
	FaultGrace time.Duration `yaml:"fault_grace"`
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
			BaseURL:      "http://192.168.1.100:8088",
			PollInterval: 5 * time.Second,
			Timeout:      10 * time.Second,
			FaultGrace:   45 * time.Minute,
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
				Enabled:       false, // observe-only, opt-in per plant
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
		CountGroups: CountGroupsConfig{
			PollInterval:       500 * time.Millisecond,
			RDSTimeout:         400 * time.Millisecond,
			OnThreshold:        2,
			OffThreshold:       3,
			FailSafeTimeout:    5 * time.Second,
			NeverOccupiedWarn:  5 * time.Minute,
			NeverOccupiedError: 30 * time.Minute,
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
