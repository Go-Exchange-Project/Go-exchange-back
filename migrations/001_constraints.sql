-- +goose Up
-- Phase 2F: minimal DB constraints for wallet, order, and trade invariants.
-- Apply after AutoMigrate has created the base tables.
--
-- Existing data must satisfy these constraints before this migration can succeed.
--
-- The original wallets/ledger_entries constraints and indexes were removed here
-- (double-entry ledger migration, Task 5): those tables no longer exist after
-- AutoMigrate (model.Wallet/model.LedgerEntry deleted) and are dropped outright
-- by migration 010. Editing this already-applied baseline is safe — goose tracks
-- applied versions by number, not content, and this project rebuilds its dev/test
-- schema from scratch rather than migrating old data forward.

ALTER TABLE trades
    ADD COLUMN IF NOT EXISTS engine_sequence bigint NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS engine_event_id varchar(128) NOT NULL DEFAULT '';

ALTER TABLE trades
    ALTER COLUMN idempotency_key SET NOT NULL,
    ALTER COLUMN engine_sequence SET NOT NULL,
    ALTER COLUMN engine_event_id SET NOT NULL,
    ALTER COLUMN coin_symbol SET NOT NULL,
    ALTER COLUMN price SET NOT NULL,
    ALTER COLUMN quantity SET NOT NULL,
    ALTER COLUMN traded_at SET NOT NULL,
    ALTER COLUMN buy_order_id SET NOT NULL,
    ALTER COLUMN sell_order_id SET NOT NULL;

ALTER TABLE orders
    ALTER COLUMN user_id SET NOT NULL,
    ALTER COLUMN coin_symbol SET NOT NULL,
    ALTER COLUMN side SET NOT NULL,
    ALTER COLUMN status SET NOT NULL,
    ALTER COLUMN order_type SET NOT NULL,
    ALTER COLUMN amount SET NOT NULL,
    ALTER COLUMN filled_amount SET NOT NULL,
    ALTER COLUMN price SET NOT NULL;

ALTER TABLE failed_settlements
    ADD COLUMN IF NOT EXISTS resolution text,
    ADD COLUMN IF NOT EXISTS resolved_by varchar(128),
    ADD COLUMN IF NOT EXISTS notes text;

ALTER TABLE failed_settlements
    ALTER COLUMN trade_idempotency_key SET NOT NULL,
    ALTER COLUMN coin_symbol SET NOT NULL,
    ALTER COLUMN buy_order_id SET NOT NULL,
    ALTER COLUMN sell_order_id SET NOT NULL,
    ALTER COLUMN price SET NOT NULL,
    ALTER COLUMN quantity SET NOT NULL,
    ALTER COLUMN error_message SET NOT NULL,
    ALTER COLUMN status SET NOT NULL,
    ALTER COLUMN retry_count SET NOT NULL,
    ALTER COLUMN occurred_at SET NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_trades_idempotency_key
    ON trades (idempotency_key);

CREATE INDEX IF NOT EXISTS idx_trades_engine_sequence
    ON trades (engine_sequence);

CREATE UNIQUE INDEX IF NOT EXISTS idx_trades_engine_event_id
    ON trades (engine_event_id)
    WHERE length(btrim(engine_event_id)) > 0;

CREATE UNIQUE INDEX IF NOT EXISTS idx_failed_settlements_trade_idempotency_key
    ON failed_settlements (trade_idempotency_key);

CREATE INDEX IF NOT EXISTS idx_orders_open_bootstrap
    ON orders (status, created_at, id);

CREATE INDEX IF NOT EXISTS idx_orders_user_status_created_at
    ON orders (user_id, status, created_at DESC, id DESC);

CREATE UNIQUE INDEX IF NOT EXISTS idx_users_email_non_empty
    ON users (lower(email))
    WHERE length(btrim(coalesce(email, ''))) > 0;

CREATE INDEX IF NOT EXISTS idx_failed_settlements_open_triage
    ON failed_settlements (status, occurred_at, id);

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'trades'::regclass
          AND conname = 'ck_trades_idempotency_key_not_empty'
    ) THEN
        ALTER TABLE trades
            ADD CONSTRAINT ck_trades_idempotency_key_not_empty CHECK (length(btrim(idempotency_key)) > 0);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'trades'::regclass
          AND conname = 'ck_trades_engine_sequence_non_negative'
    ) THEN
        ALTER TABLE trades
            ADD CONSTRAINT ck_trades_engine_sequence_non_negative CHECK (engine_sequence >= 0);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'trades'::regclass
          AND conname = 'ck_trades_price_positive'
    ) THEN
        ALTER TABLE trades
            ADD CONSTRAINT ck_trades_price_positive CHECK (price > 0);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'trades'::regclass
          AND conname = 'ck_trades_quantity_positive'
    ) THEN
        ALTER TABLE trades
            ADD CONSTRAINT ck_trades_quantity_positive CHECK (quantity > 0);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'orders'::regclass
          AND conname = 'ck_orders_amount_positive'
    ) THEN
        ALTER TABLE orders
            ADD CONSTRAINT ck_orders_amount_positive CHECK (amount > 0);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'orders'::regclass
          AND conname = 'ck_orders_filled_amount_non_negative'
    ) THEN
        ALTER TABLE orders
            ADD CONSTRAINT ck_orders_filled_amount_non_negative CHECK (filled_amount >= 0);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'orders'::regclass
          AND conname = 'ck_orders_price_non_negative'
    ) THEN
        ALTER TABLE orders
            ADD CONSTRAINT ck_orders_price_non_negative CHECK (price >= 0);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'failed_settlements'::regclass
          AND conname = 'ck_failed_settlements_trade_idempotency_key_not_empty'
    ) THEN
        ALTER TABLE failed_settlements
            ADD CONSTRAINT ck_failed_settlements_trade_idempotency_key_not_empty CHECK (length(btrim(trade_idempotency_key)) > 0);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'failed_settlements'::regclass
          AND conname = 'ck_failed_settlements_price_positive'
    ) THEN
        ALTER TABLE failed_settlements
            ADD CONSTRAINT ck_failed_settlements_price_positive CHECK (price > 0);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'failed_settlements'::regclass
          AND conname = 'ck_failed_settlements_quantity_positive'
    ) THEN
        ALTER TABLE failed_settlements
            ADD CONSTRAINT ck_failed_settlements_quantity_positive CHECK (quantity > 0);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'failed_settlements'::regclass
          AND conname = 'ck_failed_settlements_error_message_not_empty'
    ) THEN
        ALTER TABLE failed_settlements
            ADD CONSTRAINT ck_failed_settlements_error_message_not_empty CHECK (length(btrim(error_message)) > 0);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'failed_settlements'::regclass
          AND conname = 'ck_failed_settlements_status_not_empty'
    ) THEN
        ALTER TABLE failed_settlements
            ADD CONSTRAINT ck_failed_settlements_status_not_empty CHECK (length(btrim(status)) > 0);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'failed_settlements'::regclass
          AND conname = 'ck_failed_settlements_status_valid'
    ) THEN
        ALTER TABLE failed_settlements
            ADD CONSTRAINT ck_failed_settlements_status_valid CHECK (status IN ('OPEN', 'RESOLVED'));
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'failed_settlements'::regclass
          AND conname = 'ck_failed_settlements_retry_count_positive'
    ) THEN
        ALTER TABLE failed_settlements
            ADD CONSTRAINT ck_failed_settlements_retry_count_positive CHECK (retry_count > 0);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'failed_settlements'::regclass
          AND conname = 'ck_failed_settlements_resolved_requires_audit'
    ) THEN
        ALTER TABLE failed_settlements
            ADD CONSTRAINT ck_failed_settlements_resolved_requires_audit
            CHECK (
                status <> 'RESOLVED'
                OR (
                    resolved_at IS NOT NULL
                    AND length(btrim(coalesce(resolution, ''))) > 0
                )
            );
    END IF;

END $$;
-- +goose StatementEnd

-- +goose Down
-- This baseline migration is intentionally not reversible. It is written to be
-- idempotent for existing development databases, and rollback would risk
-- dropping data-bearing constraints/columns that earlier AutoMigrate already
-- created. Future migrations should provide concrete Down statements when safe.
SELECT 1;
