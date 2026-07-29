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
	Sim           SimConfig           `yaml:"sim"`
	Sourceability SourceabilityConfig `yaml:"sourceability"`
	Replenishment ReplenishmentConfig `yaml:"replenishment"`
	Logging       LoggingConfig       `yaml:"logging"`
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
	// AbandonStuck cancels orders stuck non-terminal past this age — a held
	// swap removal leg whose supply never arrives (queued), a robot parked at
	// a staging node (staged), or a leg handed to the fleet that never started
	// moving (sourcing/dispatched; the long-weekend drain case). in_transit is
	// excluded (actively moving). Cascades to the two-robot sibling.
	AbandonStuck time.Duration `yaml:"abandon_stuck"` // default 1h; 0 = disabled
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
		},
		Sourceability: SourceabilityConfig{
			EnableAtRisk: false, // green/red only until the owner validates the rate window on plant data
			RateWindow:   30 * time.Minute,
			Horizon:      30 * time.Minute,
		},
		Logging: LoggingConfig{
			StderrSubsystems: DefaultStderrSubsystems(),
		},
		Replenishment: ReplenishmentConfig{
			// R1 LIVE by default: decide off the Edge lineside reports (ledger +
			// fresh-node adjustments). Set "ledger" to revert to pure-ledger.
			LinesideDecisionMode: "edge_reports",
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
		Sim: SimConfig{
			// Enabled false by default; Seed 0 = derive+log. Sane sim timings so a
			// dev YAML can flip enabled:true without specifying every knob.
			TransitTime: 5 * time.Second,
			JitterPct:   0.2,
		},
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
