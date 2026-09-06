package promotions

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"
	"modernc.org/sqlite"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/enttest"
	"github.com/bengobox/pos-service/internal/ent/promotionrule"
)

// ── pure-Go sqlite shim (duplicated per-package, see returns/service_test.go) ──
type sqlite3Driver struct{ *sqlite.Driver }

func (d sqlite3Driver) Open(name string) (driver.Conn, error) {
	conn, err := d.Driver.Open(name)
	if err != nil {
		return nil, err
	}
	if execer, ok := conn.(interface {
		Exec(string, []driver.Value) (driver.Result, error)
	}); ok {
		if _, err := execer.Exec("PRAGMA foreign_keys = ON;", nil); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	return conn, nil
}

func init() { sql.Register("sqlite3", sqlite3Driver{Driver: &sqlite.Driver{}}) }

func newTestService(t *testing.T) (*Service, *ent.Client) {
	t.Helper()
	client := enttest.Open(t, "sqlite3", fmt.Sprintf("file:promotest_%s?mode=memory&cache=shared", uuid.NewString()))
	t.Cleanup(func() { _ = client.Close() })
	return NewService(client, zap.NewNop()), client
}

// seedTenant creates the Tenant row a test's uuid.New() tenant ID needs — Outlet.tenant carries a
// required FK edge (unlike Promotion/PromotionRule, which only store a bare tenant_id field), so
// any test that seeds an Outlet must seed its owning Tenant first with the SAME id.
func seedTenant(t *testing.T, client *ent.Client, tid uuid.UUID) *ent.Tenant {
	t.Helper()
	tenant, err := client.Tenant.Create().
		SetID(tid).
		SetName("Test Tenant").
		SetSlug("test-" + uuid.NewString()[:8]).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return tenant
}

func seedOutlet(t *testing.T, client *ent.Client, tid uuid.UUID) *ent.Outlet {
	t.Helper()
	o, err := client.Outlet.Create().
		SetTenantID(tid).
		SetTenantSlug("codevertex-demo").
		SetCode("OUT-" + uuid.NewString()[:8]).
		SetName("Test Outlet").
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed outlet: %v", err)
	}
	return o
}

// promoInput bundles the handful of fields each ApplyPromoCode test scenario varies.
type promoInput struct {
	code       string
	outletID   *uuid.UUID
	startAt    time.Time
	endAt      *time.Time
	scopeType  promotionrule.ScopeType
	scopeIDs   []string
	discType   promotionrule.DiscountType
	discValue  float64
	mealPeriod *promotionrule.MealPeriod
	noRule     bool
}

func seedPromo(t *testing.T, client *ent.Client, tid uuid.UUID, in promoInput) *ent.Promotion {
	t.Helper()
	b := client.Promotion.Create().
		SetTenantID(tid).
		SetName("Test Promo").
		SetPromoCode(in.code).
		SetStatus("active").
		SetStartAt(in.startAt)
	if in.endAt != nil {
		b = b.SetEndAt(*in.endAt)
	}
	if in.outletID != nil {
		b = b.SetOutletID(*in.outletID)
	}
	promo, err := b.Save(context.Background())
	if err != nil {
		t.Fatalf("seed promo: %v", err)
	}
	if in.noRule {
		return promo
	}
	rb := client.PromotionRule.Create().
		SetPromotionID(promo.ID).
		SetRuleType("discount").
		SetScopeType(in.scopeType).
		SetScopeIds(in.scopeIDs).
		SetDiscountType(in.discType).
		SetDiscountValue(in.discValue)
	if in.mealPeriod != nil {
		rb = rb.SetMealPeriod(*in.mealPeriod)
	}
	if _, err := rb.Save(context.Background()); err != nil {
		t.Fatalf("seed promotion rule: %v", err)
	}
	return promo
}

func codeLines() []TimedDiscountLine {
	unit := decimal.NewFromInt(1000)
	return []TimedDiscountLine{
		{DiscountLine: DiscountLine{SKU: "SKU1", Category: "Retail", Quantity: 1, UnitPrice: unit, Total: unit}},
	}
}

func TestApplyPromoCode_NotFoundOrInactive(t *testing.T) {
	svc, _ := newTestService(t)
	res, err := svc.ApplyPromoCode(context.Background(), uuid.New(), nil, "NOPE", codeLines(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Valid {
		t.Fatalf("expected invalid for unknown code, got %+v", res)
	}
	if res.Reason != "promo code not found or inactive" {
		t.Errorf("unexpected reason: %q", res.Reason)
	}
}

func TestApplyPromoCode_Expired(t *testing.T) {
	svc, client := newTestService(t)
	tid := uuid.New()
	past := time.Now().Add(-48 * time.Hour)
	seedPromo(t, client, tid, promoInput{
		code: "OLD10", startAt: time.Now().Add(-72 * time.Hour), endAt: &past,
		scopeType: promotionrule.ScopeTypeAll, discType: promotionrule.DiscountTypePercentage, discValue: 10,
	})
	res, err := svc.ApplyPromoCode(context.Background(), tid, nil, "OLD10", codeLines(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Valid || res.Reason != "promotion has expired" {
		t.Fatalf("expected expired, got %+v", res)
	}
}

func TestApplyPromoCode_NotStarted(t *testing.T) {
	svc, client := newTestService(t)
	tid := uuid.New()
	seedPromo(t, client, tid, promoInput{
		code: "FUTURE10", startAt: time.Now().Add(48 * time.Hour),
		scopeType: promotionrule.ScopeTypeAll, discType: promotionrule.DiscountTypePercentage, discValue: 10,
	})
	res, err := svc.ApplyPromoCode(context.Background(), tid, nil, "FUTURE10", codeLines(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Valid || res.Reason != "promotion has not started yet" {
		t.Fatalf("expected not-started, got %+v", res)
	}
}

func TestApplyPromoCode_OutletMismatch(t *testing.T) {
	svc, client := newTestService(t)
	tid := uuid.New()
	seedTenant(t, client, tid)
	scopedOutlet := seedOutlet(t, client, tid)
	otherOutlet := seedOutlet(t, client, tid)
	seedPromo(t, client, tid, promoInput{
		code: "OUTLETA", outletID: &scopedOutlet.ID, startAt: time.Now().Add(-time.Hour),
		scopeType: promotionrule.ScopeTypeAll, discType: promotionrule.DiscountTypePercentage, discValue: 10,
	})
	res, err := svc.ApplyPromoCode(context.Background(), tid, &otherOutlet.ID, "OUTLETA", codeLines(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Valid || res.Reason != "promotion is not available at this outlet" {
		t.Fatalf("expected outlet mismatch rejection, got %+v", res)
	}
}

func TestApplyPromoCode_NoRuleConfigured(t *testing.T) {
	svc, client := newTestService(t)
	tid := uuid.New()
	seedPromo(t, client, tid, promoInput{code: "NORULE", startAt: time.Now().Add(-time.Hour), noRule: true})
	res, err := svc.ApplyPromoCode(context.Background(), tid, nil, "NORULE", codeLines(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Valid || res.Reason != "promotion has no discount rule configured" {
		t.Fatalf("expected no-rule rejection, got %+v", res)
	}
}

// Valid, item-scoped percentage code: only the scoped SKU contributes to the base, and the whole
// codebase's rule-evaluation path (evaluateRule) is reused — not a separate flat calculator.
func TestApplyPromoCode_ValidItemScopedPercentage(t *testing.T) {
	svc, client := newTestService(t)
	tid := uuid.New()
	seedPromo(t, client, tid, promoInput{
		code: "SAVE10", startAt: time.Now().Add(-time.Hour),
		scopeType: promotionrule.ScopeTypeItem, scopeIDs: []string{"SKU1"},
		discType: promotionrule.DiscountTypePercentage, discValue: 10,
	})
	res, err := svc.ApplyPromoCode(context.Background(), tid, nil, "SAVE10", codeLines(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Valid {
		t.Fatalf("expected valid, got reason=%q", res.Reason)
	}
	if !res.DiscountAmount.Equal(decimal.NewFromInt(100)) {
		t.Fatalf("expected 10%% of 1000 = 100, got %s", res.DiscountAmount)
	}
	if res.PerSKU["SKU1"].Amount.IntPart() != 100 {
		t.Fatalf("expected per-SKU attribution, got %+v", res.PerSKU)
	}
}

// A code whose rule is meal_period-gated only credits lines added inside that period — the exact
// gap this sprint closed, now covered end-to-end through the real ApplyPromoCode entrypoint (not
// just the pure combineTimedDiscounts test).
func TestApplyPromoCode_MealPeriodGate(t *testing.T) {
	svc, client := newTestService(t)
	tid := uuid.New()
	lunch := promotionrule.MealPeriodLunch
	seedPromo(t, client, tid, promoInput{
		code: "LUNCH10", startAt: time.Now().Add(-time.Hour),
		scopeType: promotionrule.ScopeTypeItem, scopeIDs: []string{"SKU1"},
		discType: promotionrule.DiscountTypePercentage, discValue: 10, mealPeriod: &lunch,
	})
	unit := decimal.NewFromInt(1000)
	dinnerLine := TimedDiscountLine{
		DiscountLine: DiscountLine{SKU: "SKU1", Quantity: 1, UnitPrice: unit, Total: unit},
		AddedAt:      time.Date(2026, 1, 2, 20, 0, 0, 0, time.UTC), // 20:00 — dinner, not lunch
	}
	res, err := svc.ApplyPromoCode(context.Background(), tid, nil, "LUNCH10", []TimedDiscountLine{dinnerLine}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Valid {
		t.Fatalf("expected a lunch-only code to reject a line added at dinner, got %+v", res)
	}
}
