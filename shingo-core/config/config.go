package config

import (
	"os"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	mu sync.RWMutex `yaml:"-"`

	Database    DatabaseConfig    `yaml:"database"`
	RDS         RDSConfig         `yaml:"rds"`
	Web         WebConfig         `yaml:"web"`
	Messaging   MessagingConfig   `yaml:"messaging"`
	Staging     StagingConfig     `yaml:"staging"`
	CountGroups CountGroupsConfig `yaml:"count_groups"`
	FireAlarm   FireAlarmConfig   `yaml:"fire_alarm"`
	Sim         SimConfig         `yaml:"sim"`
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
