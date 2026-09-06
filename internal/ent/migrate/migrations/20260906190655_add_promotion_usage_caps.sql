-- Modify "promotions" table
ALTER TABLE "promotions" ADD COLUMN "usage_limit" bigint NULL, ADD COLUMN "max_units_per_customer" bigint NULL;
-- Create "promotion_redemptions" table
CREATE TABLE "promotion_redemptions" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "promotion_id" uuid NOT NULL, "customer_key" character varying NULL, "channel" character varying NOT NULL, "order_id" character varying NOT NULL, "quantity" double precision NOT NULL, "created_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "promotionredemption_idempotency" to table: "promotion_redemptions"
CREATE UNIQUE INDEX "promotionredemption_idempotency" ON "promotion_redemptions" ("tenant_id", "promotion_id", "channel", "order_id");
-- Create index "promotionredemption_tenant_id_promotion_id" to table: "promotion_redemptions"
CREATE INDEX "promotionredemption_tenant_id_promotion_id" ON "promotion_redemptions" ("tenant_id", "promotion_id");
-- Create index "promotionredemption_tenant_id_promotion_id_customer_key" to table: "promotion_redemptions"
CREATE INDEX "promotionredemption_tenant_id_promotion_id_customer_key" ON "promotion_redemptions" ("tenant_id", "promotion_id", "customer_key");
