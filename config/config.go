package config

import (
	"os"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	mu sync.Mutex `yaml:"-"`

	FactoryID string         `yaml:"factory_id"`
	Database  DatabaseConfig `yaml:"database"`
	Redis     RedisConfig    `yaml:"redis"`
	RDS       RDSConfig      `yaml:"rds"`
	Web       WebConfig      `yaml:"web"`
	Messaging MessagingConfig `yaml:"messaging"`
}

type DatabaseConfig struct {
	Driver   string         `yaml:"driver"`
	SQLite   SQLiteConfig   `yaml:"sqlite"`
	Postgres PostgresConfig `yaml:"postgres"`
}

type SQLiteConfig struct {
	Path string `yaml:"path"`
}

type PostgresConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Database string `yaml:"database"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	SSLMode  string `yaml:"sslmode"`
}

type RedisConfig struct {
	Address  string `yaml:"address"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
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
	Backend             string      `yaml:"backend"`
	MQTT                MQTTConfig  `yaml:"mqtt"`
	Kafka               KafkaConfig `yaml:"kafka"`
	OrdersTopic         string      `yaml:"orders_topic"`
	DispatchTopicPrefix string      `yaml:"dispatch_topic_prefix"`
	OutboxDrainInterval time.Duration `yaml:"outbox_drain_interval"`
}

type MQTTConfig struct {
	Broker   string `yaml:"broker"`
	Port     int    `yaml:"port"`
	ClientID string `yaml:"client_id"`
}

type KafkaConfig struct {
	Brokers []string `yaml:"brokers"`
}

func Defaults() *Config {
	return &Config{
		FactoryID: "plant-alpha",
		Database: DatabaseConfig{
			Driver: "sqlite",
			SQLite: SQLiteConfig{Path: "shingocore.db"},
			Postgres: PostgresConfig{
				Host:     "localhost",
				Port:     5432,
				Database: "shingocore",
				User:     "shingocore",
				Password: "",
				SSLMode:  "disable",
			},
		},
		Redis: RedisConfig{
			Address:  "localhost:6379",
			Password: "",
			DB:       0,
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
		Messaging: MessagingConfig{
			Backend: "mqtt",
			MQTT: MQTTConfig{
				Broker:   "localhost",
				Port:     1883,
				ClientID: "shingocore",
			},
			Kafka: KafkaConfig{
				Brokers: []string{},
			},
			OrdersTopic:         "shingocore/orders",
			DispatchTopicPrefix: "shingocore/dispatch",
			OutboxDrainInterval: 5 * time.Second,
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
