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

	CoreAPI   string          `yaml:"core_api"` // Core HTTP base URL (e.g. "http://192.168.1.10:8080")
	WarLink   WarLinkConfig   `yaml:"warlink"`
	Web       WebConfig       `yaml:"web"`
	Messaging MessagingConfig `yaml:"messaging"`
	Counter   CounterConfig   `yaml:"counter"`
	Backup    BackupConfig    `yaml:"backup"`
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
