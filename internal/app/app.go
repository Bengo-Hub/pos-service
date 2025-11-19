package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/config"
	handlers "github.com/bengobox/pos-service/internal/http/handlers"
	router "github.com/bengobox/pos-service/internal/http/router"
	"github.com/bengobox/pos-service/internal/platform/cache"
	"github.com/bengobox/pos-service/internal/platform/database"
	"github.com/bengobox/pos-service/internal/platform/events"
	"github.com/bengobox/pos-service/internal/shared/logger"
	authclient "github.com/Bengo-Hub/shared-auth-client"
)

type App struct {
	cfg        *config.Config
	log        *zap.Logger
	httpServer *http.Server
	db         *pgxpool.Pool
	cache      *redis.Client
	events     *nats.Conn
}

func New(ctx context.Context) (*App, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}

	log, err := logger.New(cfg.App.Env)
	if err != nil {
		return nil, fmt.Errorf("logger init: %w", err)
	}

	dbPool, err := database.NewPool(ctx, cfg.Postgres)
	if err != nil {
		return nil, fmt.Errorf("postgres init: %w", err)
	}

	redisClient := cache.NewClient(cfg.Redis)

	natsConn, err := events.Connect(cfg.Events)
	if err != nil {
		log.Warn("event bus connection failed", zap.Error(err))
	}

	healthHandler := handlers.NewHealthHandler(log, dbPool, redisClient, natsConn)

	// Initialize auth-service JWT validator
	var authMiddleware *authclient.AuthMiddleware
	authConfig := authclient.DefaultConfig(
		cfg.Auth.JWKSUrl,
		cfg.Auth.Issuer,
		cfg.Auth.Audience,
	)
	authConfig.CacheTTL = cfg.Auth.JWKSCacheTTL
	authConfig.RefreshInterval = cfg.Auth.JWKSRefreshInterval

	validator, err := authclient.NewValidator(authConfig)
	if err != nil {
		return nil, fmt.Errorf("auth validator init: %w", err)
	}
	authMiddleware = authclient.NewAuthMiddleware(validator)

	chiRouter := router.New(log, healthHandler, authMiddleware)

	httpServer := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.HTTP.Host, cfg.HTTP.Port),
		Handler:           chiRouter,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
	}

	return &App{
		cfg:        cfg,
		log:        log,
		httpServer: httpServer,
		db:         dbPool,
		cache:      redisClient,
		events:     natsConn,
	}, nil
}

func (a *App) Run(ctx context.Context) error {
	a.log.Info("pos service starting", zap.String("addr", a.httpServer.Addr))

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.httpServer.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := a.httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("http shutdown: %w", err)
		}

		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("http server error: %w", err)
	}
}

func (a *App) Close() {
	if a.events != nil {
		if err := a.events.Drain(); err != nil {
			a.log.Warn("nats drain failed", zap.Error(err))
		}
		a.events.Close()
	}

	if a.cache != nil {
		if err := a.cache.Close(); err != nil {
			a.log.Warn("redis close failed", zap.Error(err))
		}
	}

	if a.db != nil {
		a.db.Close()
	}

	_ = a.log.Sync()
}

