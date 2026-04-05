package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/spf13/viper"
)

// ─────────────────────────────────────────
// Struct Tanımları
// ─────────────────────────────────────────

type Config struct {
	ClusterID string        `mapstructure:"clusterId"`
	Backend   BackendConfig `mapstructure:"backend"`
	Watch     WatchConfig   `mapstructure:"watch"`
	Logs      LogConfig     `mapstructure:"logs"`
	Buffer    BufferConfig  `mapstructure:"buffer"`
	GRPC      GRPCConfig    `mapstructure:"grpc"`
}

type BackendConfig struct {
	Host  string `mapstructure:"host"`
	Port  int    `mapstructure:"port"`
	TLS   bool   `mapstructure:"tls"`
	Token string `mapstructure:"token"`
}

type WatchConfig struct {
	Namespaces     []NamespaceSelector `mapstructure:"namespaces"`
	ResyncInterval time.Duration       `mapstructure:"resyncInterval"`
}

type NamespaceSelector struct {
	Namespace     string `mapstructure:"namespace"`
	LabelSelector string `mapstructure:"labelSelector"`
}

type LogConfig struct {
	Enabled    bool        `mapstructure:"enabled"`
	BufferSize int         `mapstructure:"bufferSize"`
	Targets    []LogTarget `mapstructure:"targets"`
}

type LogTarget struct {
	Namespace      string   `mapstructure:"namespace"`
	LabelSelector  string   `mapstructure:"labelSelector"`
	ContainerNames []string `mapstructure:"containerNames"`
}

type BufferConfig struct {
	Memory MemoryBufferSettings `mapstructure:"memory"`
	Redis  RedisConfig          `mapstructure:"redis"`
}

// MemoryBufferSettings backs the in-process queue before Redis spillover.
type MemoryBufferSettings struct {
	// Capacity max items held in memory; 0 uses logs.bufferSize.
	Capacity int `mapstructure:"capacity"`
	// MaxAttempts passed to MemoryBuffer (Nack requeue until exceeded).
	MaxAttempts int `mapstructure:"maxAttempts"`
}

type RedisConfig struct {
	Enabled  bool   `mapstructure:"enabled"`
	Host     string `mapstructure:"host"`
	Port     int    `mapstructure:"port"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
}

type GRPCConfig struct {
	DialTimeout      time.Duration `mapstructure:"dialTimeout"`
	KeepaliveTime    time.Duration `mapstructure:"keepaliveTime"`
	KeepaliveTimeout time.Duration `mapstructure:"keepaliveTimeout"`
	MaxRetries       int           `mapstructure:"maxRetries"`
	RetryBaseDelay   time.Duration `mapstructure:"retryBaseDelay"`
	RetryMaxDelay    time.Duration `mapstructure:"retryMaxDelay"`
}

// ─────────────────────────────────────────
// Load
// ─────────────────────────────────────────

// Load reads config using the standard search paths: /etc/kubexa/config.yaml, then ./config/config.yaml.
func Load() (*Config, error) {
	return LoadFrom("")
}

// LoadFrom reads YAML config. If configPath is non-empty, that file is used. Otherwise Load uses
// the same discovery as Load() (named "config" under /etc/kubexa and ./config).
func LoadFrom(configPath string) (*Config, error) {
	v := viper.New()

	v.SetEnvPrefix("KUBEXA")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	_ = v.BindEnv("backend.token", "KUBEXA_GRPC_TOKEN")
	_ = v.BindEnv("buffer.redis.password", "KUBEXA_REDIS_PASSWORD")

	setDefaults(v)

	if configPath != "" {
		v.SetConfigFile(configPath)
	} else {
		v.SetConfigName("config")
		v.SetConfigType("yaml")
		v.AddConfigPath("/etc/kubexa")
		v.AddConfigPath("./config")
	}

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("config read: %w", err)
	}

	log.Info().
		Str("file", v.ConfigFileUsed()).
		Msg("config loaded")

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("config parse error: %w", err)
	}

	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("config invalid: %w", err)
	}

	return &cfg, nil
}

// ─────────────────────────────────────────
// Defaults
// ─────────────────────────────────────────

func setDefaults(v *viper.Viper) {
	// backend
	v.SetDefault("backend.port", 443)
	v.SetDefault("backend.tls", true)

	// watch
	v.SetDefault("watch.resyncInterval", 30*time.Second)

	// logs
	v.SetDefault("logs.enabled", true)
	v.SetDefault("logs.bufferSize", 1000)

	// buffer
	v.SetDefault("buffer.memory.capacity", 0)
	v.SetDefault("buffer.memory.maxAttempts", 5)
	v.SetDefault("buffer.redis.enabled", false)
	v.SetDefault("buffer.redis.host", "localhost")
	v.SetDefault("buffer.redis.port", 6379)
	v.SetDefault("buffer.redis.db", 0)

	// grpc
	v.SetDefault("grpc.dialTimeout", 10*time.Second)
	// Default below typical server enforcement MinTime (~5m); avoids ENHANCE_YOUR_CALM / too_many_pings.
	v.SetDefault("grpc.keepaliveTime", 5*time.Minute)
	v.SetDefault("grpc.keepaliveTimeout", 20*time.Second)
	v.SetDefault("grpc.maxRetries", 10)
	v.SetDefault("grpc.retryBaseDelay", 1*time.Second)
	v.SetDefault("grpc.retryMaxDelay", 60*time.Second)
}

// ─────────────────────────────────────────
// Validate
// ─────────────────────────────────────────

func validate(cfg *Config) error {
	if cfg.ClusterID == "" {
		return fmt.Errorf("clusterId cannot be empty")
	}
	if cfg.Backend.Host == "" {
		return fmt.Errorf("backend.host cannot be empty")
	}
	if cfg.Backend.Port <= 0 || cfg.Backend.Port > 65535 {
		return fmt.Errorf("backend.port invalid: %d", cfg.Backend.Port)
	}
	if len(cfg.Watch.Namespaces) == 0 {
		return fmt.Errorf("at least one watch.namespace must be defined")
	}
	return nil
}

// ─────────────────────────────────────────
// Helper
// ─────────────────────────────────────────

// Address returns the backend's host:port address
func (c *BackendConfig) Address() string {
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}

// RedisAddress returns the redis's host:port address
func (r *RedisConfig) Address() string {
	return fmt.Sprintf("%s:%d", r.Host, r.Port)
}
