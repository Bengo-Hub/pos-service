package config

import (
	"fmt"
	"time"

	"github.com/joho/godotenv"
	"github.com/kelseyhightower/envconfig"
)

const namespace = "POS"

// Config aggregates runtime configuration for the POS service.
type Config struct {
	App       AppConfig
	HTTP      HTTPConfig
	Postgres  PostgresConfig
	Redis     RedisConfig
	Events    EventsConfig
	Telemetry TelemetryConfig
	Auth      AuthConfig
}

type AppConfig struct {
	Name    string `envconfig:"APP_NAME" default:"pos-service"`
	Env     string `envconfig:"APP_ENV" default:"development"`
	Region  string `envconfig:"APP_REGION" default:"africa-east-1"`
	Version string `envconfig:"APP_VERSION" default:"0.1.0"`
}

type HTTPConfig struct {
	Host         string        `envconfig:"HTTP_HOST" default:"0.0.0.0"`
	Port         int           `envconfig:"HTTP_PORT" default:"4004"`
	ReadTimeout  time.Duration `envconfig:"HTTP_READ_TIMEOUT" default:"20s"`
	WriteTimeout time.Duration `envconfig:"HTTP_WRITE_TIMEOUT" default:"20s"`
	IdleTimeout  time.Duration `envconfig:"HTTP_IDLE_TIMEOUT" default:"90s"`
}

type PostgresConfig struct {
	URL             string        `envconfig:"POSTGRES_URL" default:"postgres://postgres:postgres@localhost:5432/pos?sslmode=disable"`
	MaxOpenConns    int           `envconfig:"POSTGRES_MAX_OPEN_CONNS" default:"30"`
	MaxIdleConns    int           `envconfig:"POSTGRES_MAX_IDLE_CONNS" default:"15"`
	ConnMaxLifetime time.Duration `envconfig:"POSTGRES_CONN_MAX_LIFETIME" default:"45m"`
}

type RedisConfig struct {
	Addr        string        `envconfig:"REDIS_ADDR" default:"localhost:6380"`
	Username    string        `envconfig:"REDIS_USERNAME"`
	Password    string        `envconfig:"REDIS_PASSWORD"`
	DB          int           `envconfig:"REDIS_DB" default:"0"`
	TLSRequired bool          `envconfig:"REDIS_TLS_REQUIRED" default:"false"`
	DialTimeout time.Duration `envconfig:"REDIS_DIAL_TIMEOUT" default:"5s"`
}

type EventsConfig struct {
	Bus           string `envconfig:"EVENT_BUS" default:"nats"`
	NATSURL       string `envconfig:"NATS_URL" default:"nats://localhost:4222"`
	StreamName    string `envconfig:"NATS_STREAM" default:"pos"`
	DeliverGroup  string `envconfig:"NATS_DELIVER_GROUP" default:"pos-workers"`
	DeadLetterJet string `envconfig:"NATS_DLQ_STREAM" default:"pos-dlq"`
}

type TelemetryConfig struct {
	OTLPEndpoint string `envconfig:"OTLP_ENDPOINT"`
	MetricsURL   string `envconfig:"METRICS_ENDPOINT"`
	TracingURL   string `envconfig:"TRACING_ENDPOINT"`
}

type AuthConfig struct {
	// Auth Service SSO (JWT) integration
	ServiceURL          string        `envconfig:"AUTH_SERVICE_URL" default:"https://auth.codevertex.local:4101"`
	Issuer              string        `envconfig:"AUTH_ISSUER" default:"https://auth.codevertex.local:4101"`
	Audience            string        `envconfig:"AUTH_AUDIENCE" default:"bengobox"`
	JWKSUrl             string        `envconfig:"AUTH_JWKS_URL" default:"https://auth.codevertex.local:4101/api/v1/.well-known/jwks.json"`
	JWKSCacheTTL        time.Duration `envconfig:"AUTH_JWKS_CACHE_TTL" default:"3600s"`
	JWKSRefreshInterval  time.Duration `envconfig:"AUTH_JWKS_REFRESH_INTERVAL" default:"300s"`
}

// Load gathers configuration from environment variables and optional .env files.
func Load() (*Config, error) {
	_ = godotenv.Load()

	var cfg Config
	if err := envconfig.Process(namespace, &cfg); err != nil {
		return nil, fmt.Errorf("config: failed to load environment variables: %w", err)
	}

	return &cfg, nil
}

