package subscriptions

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	notifmod "github.com/bengobox/pos-service/internal/modules/notifications"
)

// CacheSubscriber listens for tenant.subscription.updated events (plan changes, suspensions, and
// TenantFeatureGrant add-on grants/revokes) and (1) invalidates the shared tenant branding/
// metadata cache so downstream reads pick up new plan data, and (2) pushes a real-time
// "entitlements_changed" nudge over the same notification hub pos-ui already holds open for
// catalog pushes, so an already-open terminal/back-office tab reflects a revoke/grant/plan-change
// within seconds instead of only on its next full page load.
type CacheSubscriber struct {
	redis    *redis.Client
	logger   *zap.Logger
	sub      *nats.Subscription
	notifHub *notifmod.Hub
}

// NewCacheSubscriber creates a CacheSubscriber.
func NewCacheSubscriber(redisClient *redis.Client, logger *zap.Logger) *CacheSubscriber {
	return &CacheSubscriber{
		redis:  redisClient,
		logger: logger.Named("subscriptions.cache-subscriber"),
	}
}

// SetNotifHub wires the real-time push hub (optional — nil degrades to cache-invalidation-only,
// the pre-existing behavior). Call before Start.
func (s *CacheSubscriber) SetNotifHub(hub *notifmod.Hub) { s.notifHub = hub }

// Start subscribes to tenant.subscription.updated on the provided NATS connection.
func (s *CacheSubscriber) Start(conn *nats.Conn) error {
	// QueueSubscribe so a single pod invalidates the cache per event across replicas.
	sub, err := conn.QueueSubscribe("tenant.subscription.updated", "pos-subcache", s.handle)
	if err != nil {
		return err
	}
	s.sub = sub
	s.logger.Info("subscribed to tenant.subscription.updated (queue group pos-subcache)")
	return nil
}

// Stop drains the NATS subscription.
func (s *CacheSubscriber) Stop() {
	if s.sub != nil {
		_ = s.sub.Drain()
	}
}

func (s *CacheSubscriber) handle(msg *nats.Msg) {
	var wrapper struct {
		TenantID   uuid.UUID              `json:"tenant_id,omitempty"`
		TenantSlug string                 `json:"tenant_slug,omitempty"`
		Payload    map[string]interface{} `json:"payload"`
	}
	if err := json.Unmarshal(msg.Data, &wrapper); err != nil {
		s.logger.Warn("failed to parse subscription.updated event", zap.Error(err))
		return
	}

	slug := wrapper.TenantSlug
	if slug == "" {
		if v, ok := wrapper.Payload["tenant_slug"].(string); ok {
			slug = v
		}
	}
	if slug == "" {
		s.logger.Warn("subscription.updated event missing tenant_slug, skipping")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cacheKey := "tenant:" + slug
	if err := s.redis.Del(ctx, cacheKey).Err(); err != nil {
		s.logger.Warn("failed to invalidate tenant cache",
			zap.String("key", cacheKey),
			zap.Error(err),
		)
	} else {
		s.logger.Debug("invalidated tenant cache on subscription update", zap.String("key", cacheKey))
	}

	// Real-time push: nudge every open POS/back-office tab for this tenant to refetch its
	// entitlements. Best-effort — a missing/nil TenantID (a malformed or legacy event) just skips
	// the push, same fail-open posture as the rest of this best-effort cache-invalidation subscriber.
	if s.notifHub != nil && wrapper.TenantID != uuid.Nil {
		s.notifHub.BroadcastToTenant(wrapper.TenantID, notifmod.Message{
			Type:    "entitlements_changed",
			Payload: map[string]any{"tenant_id": wrapper.TenantID},
		})
	}
}
