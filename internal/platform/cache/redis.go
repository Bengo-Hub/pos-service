package cache

import (
	"crypto/tls"

	"github.com/redis/go-redis/v9"

	"github.com/bengobox/pos-service/internal/config"
)

func NewClient(cfg config.RedisConfig) *redis.Client {
	options := &redis.Options{
		Addr:        cfg.Addr,
		Username:    cfg.Username,
		Password:    cfg.Password,
		DB:          cfg.DB,
		DialTimeout: cfg.DialTimeout,
	}

	if cfg.TLSRequired {
		options.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	return redis.NewClient(options)
}

