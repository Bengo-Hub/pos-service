package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

type dbPinger interface {
	Ping(context.Context) error
}

// HealthHandler exposes liveness/readiness endpoints for the POS service.
type HealthHandler struct {
	log    *zap.Logger
	db     dbPinger
	cache  *redis.Client
	events *nats.Conn
}

func NewHealthHandler(log *zap.Logger, db dbPinger, cache *redis.Client, events *nats.Conn) *HealthHandler {
	return &HealthHandler{log: log, db: db, cache: cache, events: events}
}

type livenessResponse struct {
	Status  string `json:"status" example:"ok"`
	Service string `json:"service" example:"pos-service"`
}

type readinessResponse struct {
	Status       string            `json:"status" example:"OK"`
	Dependencies map[string]string `json:"dependencies"`
}

// Liveness reports whether the API process is running.
// @Summary Service liveness probe
// @Description Returns OK when the POS API process is running.
// @Tags Health
// @Produce json
// @Success 200 {object} livenessResponse
// @Router /healthz [get]
func (h *HealthHandler) Liveness(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, livenessResponse{
		Status:  "ok",
		Service: "pos-service",
	})
}

// Readiness checks downstream infrastructure dependencies.
// @Summary Readiness probe
// @Description Validates connectivity to Postgres, Redis, and NATS backends.
// @Tags Health
// @Produce json
// @Success 200 {object} readinessResponse
// @Failure 503 {object} readinessResponse
// @Router /readyz [get]
func (h *HealthHandler) Readiness(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	issues := map[string]string{}

	if h.db != nil {
		if err := h.db.Ping(ctx); err != nil {
			issues["postgres"] = err.Error()
		}
	}

	if h.cache != nil {
		if err := h.cache.Ping(ctx).Err(); err != nil {
			issues["redis"] = err.Error()
		}
	}

	if h.events != nil && !h.events.IsConnected() {
		issues["nats"] = "not connected"
	}

	status := http.StatusOK
	if len(issues) > 0 {
		status = http.StatusServiceUnavailable
	}

	respondJSON(w, status, readinessResponse{
		Status:       http.StatusText(status),
		Dependencies: issues,
	})
}

// Metrics exposes Prometheus metrics.
// @Summary Prometheus metrics
// @Description Exposes Prometheus metrics for scraping.
// @Tags Health
// @Produce plain
// @Success 200 {string} string "Prometheus metrics payload"
// @Router /metrics [get]
func (h *HealthHandler) Metrics(w http.ResponseWriter, r *http.Request) {
	promhttp.Handler().ServeHTTP(w, r)
}

func respondJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

