package config

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level application configuration.
type Config struct {
	mu sync.RWMutex `yaml:"-"`

	Namespace    string        `yaml:"namespace"`
	LineID       string        `yaml:"line_id"`
	DatabasePath string        `yaml:"database_path"`
	PollRate     time.Duration `yaml:"poll_rate"`

	Timezone string `yaml:"timezone"` // IANA timezone for shift/hourly bucketing (e.g. "America/Chicago")

	CoreAPI     string            `yaml:"core_api"` // Core HTTP base URL (e.g. "http://192.168.1.10:8080")
	WarLink     WarLinkConfig     `yaml:"warlink"`
	Web         WebConfig         `yaml:"web"`
	Messaging   MessagingConfig   `yaml:"messaging"`
	Counter     CounterConfig     `yaml:"counter"`
	Backup      BackupConfig      `yaml:"backup"`
	CountGroups CountGroupsConfig `yaml:"count_groups"`
	Sim         SimConfig         `yaml:"sim"`
}

// CountGroupsConfig holds the edge side of the advanced-zone light feature.
// Unresolved bindings produce a startup WARN but don't block the handler —
// commands for unbound groups log and return.
//
// Heartbeat is a single shared tag; all configured bindings must live on
// HeartbeatPLC for v1. Multi-PLC support is a v2 candidate.
type CountGroupsConfig struct {
	HeartbeatInterval time.Duration      `yaml:"heartbeat_interval"`
	HeartbeatTag      string             `yaml:"heartbeat_tag"`
	HeartbeatPLC      string             `yaml:"heartbeat_plc"`
	AckWarn           time.Duration      `yaml:"ack_warn"`
	AckDead           time.Duration      `yaml:"ack_dead"`
	Codes             map[string]int     `yaml:"codes"`    // desired state -> DINT action code
	Bindings          map[string]Binding `yaml:"bindings"` // group name -> plc+request_tag
}

// Binding resolves a group name (used by core) to the PLC + tag pair
// WarLink talks to.
type Binding struct {
	PLC        string `yaml:"plc"`
	RequestTag string `yaml:"request_tag"`
}

// WarLinkConfig defines the WarLink connection.
type WarLinkConfig struct {
	Host     string        `yaml:"host"        json:"host"`
	Port     int           `yaml:"port"        json:"port"`
	PollRate time.Duration `yaml:"poll_rate"   json:"poll_rate"`
	Enabled  bool          `yaml:"enabled"     json:"enabled"`
	Mode     string        `yaml:"mode"        json:"mode"` // "sse" (default) or "poll"
}

// WebConfig defines the web server settings.
type WebConfig struct {
	Host          string `yaml:"host"`
	Port          int    `yaml:"port"`
	SessionSecret string `yaml:"session_secret"`
	AutoConfirm   bool   `yaml:"auto_confirm"`
}

// MessagingConfig defines the messaging backend.
type MessagingConfig struct {
	Kafka               KafkaConfig   `yaml:"kafka"`
	DispatchTopic       string        `yaml:"dispatch_topic"`
	OrdersTopic         string        `yaml:"orders_topic"`
	OutboxDrainInterval time.Duration `yaml:"outbox_drain_interval"`
	StationID           string        `yaml:"station_id"`
	SigningKey          string        `yaml:"signing_key"` // optional HMAC-SHA256 shared secret for envelope signing
}

// KafkaConfig defines Kafka broker settings.
type KafkaConfig struct {
	Brokers []string `yaml:"brokers"`
	GroupID string   `yaml:"group_id"`
}

// CounterConfig defines counter anomaly thresholds.
type CounterConfig struct {
	JumpThreshold int64 `yaml:"jump_threshold"`
}

// BackupConfig defines edge backup behavior and storage.
type BackupConfig struct {
	Enabled          bool           `yaml:"enabled" json:"enabled"`
	ScheduleInterval time.Duration  `yaml:"schedule_interval" json:"schedule_interval"`
	KeepHourly       int            `yaml:"keep_hourly" json:"keep_hourly"`
	KeepDaily        int            `yaml:"keep_daily" json:"keep_daily"`
	KeepWeekly       int            `yaml:"keep_weekly" json:"keep_weekly"`
	KeepMonthly      int            `yaml:"keep_monthly" json:"keep_monthly"`
	S3               BackupS3Config `yaml:"s3" json:"s3"`
}

// BackupS3Config defines an S3-compatible storage target.
type BackupS3Config struct {
	Endpoint              string `yaml:"endpoint" json:"endpoint"`
	Bucket                string `yaml:"bucket" json:"bucket"`
	Region                string `yaml:"region" json:"region"`
	AccessKey             string `yaml:"access_key" json:"access_key"`
	SecretKey             string `yaml:"secret_key" json:"secret_key"`
	UsePathStyle          bool   `yaml:"use_path_style" json:"use_path_style"`
	InsecureSkipTLSVerify bool   `yaml:"insecure_skip_tls_verify" json:"insecure_skip_tls_verify"`
}

// SimConfig configures the local-dev production/operator simulation (edge side).
// Sim code is behind //go:build sim AND requires SHINGO_ALLOW_SIM=1 at runtime;
// this struct only carries the knobs. See implementation-brief.md.
type SimConfig struct {
	Enabled    bool               `yaml:"enabled"`
	Seed       int64              `yaml:"seed"`        // PRNG seed; 0 = derive from time and log it
	Speed      float64            `yaml:"speed"`       // time multiplier: 2.0 = twice as fast. Default 1.0
	MaxSpeed   float64            `yaml:"max_speed"`   // effective-speed cap; <=0 → default (15×). Past this the clock outruns the real choreography and the loop wedges, so requests are clamped. Must match core. Set very high to effectively uncap.
	Epoch      time.Time          `yaml:"epoch"`       // sim clock start (fast-forward origin). Zero = wall-now
	AnchorWall time.Time          `yaml:"anchor_wall"` // SHARED wall anchor for fast-forward sync: sim-now = epoch + speed×(wallNow−anchor). Set IDENTICALLY in core+edge to the run-start wall time so the two clocks stay in lockstep (no cross-process drift). Zero = per-process boot anchor (drifts).
	Calendar   SimCalendarConfig  `yaml:"calendar"`
	Downtime   SimDowntimeConfig  `yaml:"downtime"`
	Processes  []SimProcessConfig `yaml:"processes"`
	Operators  SimOperatorsConfig `yaml:"operators"`
}

// SimCalendarConfig defines the production calendar for the sim (G14).
// When enabled, the readiness gate also checks shift boundaries — cells
// don't cycle during breaks, between shifts, or on weekends.
type SimCalendarConfig struct {
	Enabled bool             `yaml:"enabled"` // default false (backward-compatible)
	Shifts  []SimShiftConfig `yaml:"shifts"`  // ordered by start time; default 3×8h
	Weekend []time.Weekday   `yaml:"weekend"` // days with no production; default [Saturday, Sunday]
}

// SimShiftConfig defines one shift in the production calendar.
type SimShiftConfig struct {
	Start string           `yaml:"start"`  // "HH:MM" 24h, e.g. "06:00"
	End   string           `yaml:"end"`    // "HH:MM" 24h, e.g. "14:00"
	Break []SimBreakConfig `yaml:"breaks"` // breaks within this shift
}

// SimBreakConfig defines a break within a shift.
type SimBreakConfig struct {
	Start string `yaml:"start"` // "HH:MM" 24h
	End   string `yaml:"end"`   // "HH:MM" 24h
}

// SimDowntimeConfig configures the clustered-random downtime model (G9, §3.1).
// Per-machine 85% uptime via exponential TBF + bounded-random MTTR draws.
// Disabled by default (backward-compatible).
type SimDowntimeConfig struct {
	Enabled  bool                       `yaml:"enabled"`  // default false
	Machines []SimDowntimeMachineConfig `yaml:"machines"` // per-machine knobs; empty = disabled
}

// SimDowntimeMachineConfig defines downtime parameters for one machine (PLC).
// Availability = MTBF / (MTBF + MTTR). With MTTR = random[min, max], the
// TBF is derived to hit the target availability.
type SimDowntimeMachineConfig struct {
	PLCName      string  `yaml:"plc_name"`
	Availability float64 `yaml:"availability"` // target availability (0-1), default 0.85
	MinMTTR      string  `yaml:"min_mttr"`     // minimum repair time, e.g. "5m"
	MaxMTTR      string  `yaml:"max_mttr"`     // maximum repair time, e.g. "30m"
}

// Scaled divides a duration by the speed multiplier. Zero or negative speed is
// treated as 1.0 (no scaling). Used to scale tick intervals, operator delays,
// transit times, etc. consistently across the sim.
func (s SimConfig) Scaled(d time.Duration) time.Duration {
	if s.Speed <= 0 {
		return d
	}
	return time.Duration(float64(d) / s.Speed)
}

// SimProcessConfig describes one fake PLC counter the fake WarLink advances.
// PLCName/TagName must exactly match a reporting_points row the seed tool creates.
type SimProcessConfig struct {
	PLCName      string        `yaml:"plc_name"`
	TagName      string        `yaml:"tag_name"`
	TickInterval time.Duration `yaml:"tick_interval"` // default 3s (applied at consumption)
	UOPPerTick   int64         `yaml:"uop_per_tick"`  // default 1 (applied at consumption)
}

// SimOperatorsConfig configures the auto-operator (loader auto-LOAD, unloader
// auto-CLEAR, changeover auto-cutover). Global enable only; per-node override is v2 (Q6).
type SimOperatorsConfig struct {
	Enabled               bool          `yaml:"enabled"`
	LoaderAutoLoad        time.Duration `yaml:"loader_auto_load"`        // default 5s
	UnloaderAutoClear     time.Duration `yaml:"unloader_auto_clear"`     // default 8s
	ChangeoverAutoCutover bool          `yaml:"changeover_auto_cutover"` // default true (T3.2)
	CutoverDelay          time.Duration `yaml:"cutover_delay"`           // default 10s
}

// Defaults returns a Config with sane defaults.
func Defaults() *Config {
	return &Config{
		Namespace:    "plant-a",
		LineID:       "line-1",
		DatabasePath: "shingoedge.db",
		PollRate:     time.Second,
		WarLink: WarLinkConfig{
			Host:     "localhost",
			Port:     8080,
			PollRate: 2 * time.Second,
			Enabled:  true,
			Mode:     "sse",
		},
		Web: WebConfig{
			Host:          "0.0.0.0",
			Port:          8081,
			SessionSecret: generateSecret(),
		},
		Messaging: MessagingConfig{
			DispatchTopic:       "shingo.dispatch",
			OrdersTopic:         "shingo.orders",
			OutboxDrainInterval: 5 * time.Second,
			Kafka: KafkaConfig{
				Brokers: []string{},
			},
		},
		Counter: CounterConfig{
			JumpThreshold: 1000,
		},
		Backup: BackupConfig{
			Enabled:          false,
			ScheduleInterval: time.Hour,
			KeepHourly:       48,
			KeepDaily:        14,
			KeepWeekly:       8,
			KeepMonthly:      12,
			S3: BackupS3Config{
				Region:       "us-east-1",
				UsePathStyle: true,
			},
		},
		CountGroups: CountGroupsConfig{
			HeartbeatInterval: 1 * time.Second,
			AckWarn:           2 * time.Second,
			AckDead:           10 * time.Second,
			Codes: map[string]int{
				"on":  1,
				"off": 2,
			},
			Bindings: map[string]Binding{},
		},
		Sim: SimConfig{
			// Enabled false by default. Sim operator timings default here so a dev
			// YAML can enable sim without spelling out every knob; per-process
			// TickInterval/UOPPerTick default at consumption (fake WarLink).
			Operators: SimOperatorsConfig{
				LoaderAutoLoad:        5 * time.Second,
				UnloaderAutoClear:     8 * time.Second,
				ChangeoverAutoCutover: true,
				CutoverDelay:          10 * time.Second,
			},
		},
	}
}

// Load reads a YAML config file. If the file doesn't exist, defaults are used.
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
	// Ensure session secret is never empty (YAML may have omitted it)
	if cfg.Web.SessionSecret == "" {
		cfg.Web.SessionSecret = generateSecret()
	}
	return cfg, nil
}

// Save writes the config to a YAML file.
func (c *Config) Save(path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// stationID returns the station ID without locking (for internal use).
func (c *Config) stationID() string {
	if c.Messaging.StationID != "" {
		return c.Messaging.StationID
	}
	return c.Namespace + "." + c.LineID
}

// StationID returns the configured station ID, or derives one from namespace.line_id.
func (c *Config) StationID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.stationID()
}

// KafkaGroupID returns the Kafka consumer group ID for this edge.
// If not explicitly configured, derives a unique group from the station ID
// so that each edge receives all messages on its subscribed topics.
func (c *Config) KafkaGroupID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.Messaging.Kafka.GroupID != "" {
		return c.Messaging.Kafka.GroupID
	}
	return "shingo-edge-" + c.stationID()
}

// Lock acquires the config mutex for multi-step mutations.
func (c *Config) Lock() { c.mu.Lock() }

// Unlock releases the config mutex.
func (c *Config) Unlock() { c.mu.Unlock() }

// RLock acquires the config read lock.
func (c *Config) RLock() { c.mu.RLock() }

// RUnlock releases the config read lock.
func (c *Config) RUnlock() { c.mu.RUnlock() }

// generateSecret returns a random 32-byte hex-encoded string for session signing.
func generateSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "shingo-edge-fallback-secret"
	}
	return hex.EncodeToString(b)
}
