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
}

// CountGroupsConfig configures the advanced-zone polling feature.
// Empty Groups slice ⇒ feature disabled.
// All fields are overridable per-deployment via shingocore.yaml.
type CountGroupsConfig struct {
	PollInterval       time.Duration       `yaml:"poll_interval"`
	RDSTimeout         time.Duration       `yaml:"rds_timeout"`
	OnThreshold        int                 `yaml:"on_threshold"`
	OffThreshold       int                 `yaml:"off_threshold"`
	FailSafeTimeout    time.Duration       `yaml:"fail_safe_timeout"`
	NeverOccupiedWarn  time.Duration       `yaml:"never_occupied_warn"`
	NeverOccupiedError time.Duration       `yaml:"never_occupied_error"`
	Groups             []CountGroupConfig  `yaml:"groups"`
}

type CountGroupConfig struct {
	Name    string `yaml:"name"`
	Enabled bool   `yaml:"enabled"`
}

type FireAlarmConfig struct {
	Enabled           bool `yaml:"enabled"`             // feature gate; false = hidden from UI
	AutoResumeDefault bool `yaml:"auto_resume_default"` // default checkbox state for auto-resume on clear
}

type StagingConfig struct {
	// TTL is the global default staging expiry. 0 (the default) means permanent:
	// staged bins never auto-unstage — they're released only by the next claim
	// or by operator action. Override per-node via the `staging_ttl` property
	// (admin UI) on a specific node or its parent.
	TTL                  time.Duration `yaml:"ttl"`                    // default 0 (permanent)
	SweepInterval        time.Duration `yaml:"sweep_interval"`         // default 5m
	AutoConfirmDelivered time.Duration `yaml:"auto_confirm_delivered"` // 0 = disabled
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
			TTL:                  0,                    // 0 = never auto-unstage; override per node group via staging_ttl property
			SweepInterval:        5 * time.Minute,
			AutoConfirmDelivered: 5 * time.Minute, // auto-confirm delivered orders after 5 minutes if no receipt from Edge
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
