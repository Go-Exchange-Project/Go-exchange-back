-- Phase 2F: minimal DB constraints for wallet, order, and trade invariants.
-- Apply after AutoMigrate has created the base tables.
--
-- Existing data must satisfy these constraints before this migration can succeed.

ALTER TABLE wallets
    ALTER COLUMN user_id SET NOT NULL,
    ALTER COLUMN coin_symbol SET NOT NULL,
    ALTER COLUMN krw SET NOT NULL,
    ALTER COLUMN quantity SET NOT NULL,
    ALTER COLUMN available_balance SET NOT NULL,
    ALTER COLUMN locked_balance SET NOT NULL;

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

ALTER TABLE ledger_entries
    ALTER COLUMN user_id SET NOT NULL,
    ALTER COLUMN coin_symbol SET NOT NULL,
    ALTER COLUMN entry_type SET NOT NULL,
    ALTER COLUMN available_delta SET NOT NULL,
    ALTER COLUMN locked_delta SET NOT NULL,
    ALTER COLUMN available_balance_after SET NOT NULL,
    ALTER COLUMN locked_balance_after SET NOT NULL,
    ALTER COLUMN reference_type SET NOT NULL,
    ALTER COLUMN reference_id SET NOT NULL,
    ALTER COLUMN created_at SET NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_wallets_user_id_coin_symbol
    ON wallets (user_id, coin_symbol);

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

CREATE INDEX IF NOT EXISTS idx_ledger_entries_user_asset_created_at
    ON ledger_entries (user_id, coin_symbol, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_ledger_entries_type_reference
    ON ledger_entries (entry_type, reference_type, reference_id);

CREATE INDEX IF NOT EXISTS idx_ledger_entries_reference_key
    ON ledger_entries (reference_key);

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'wallets'::regclass
          AND conname = 'ck_wallets_krw_non_negative'
    ) THEN
        ALTER TABLE wallets
            ADD CONSTRAINT ck_wallets_krw_non_negative CHECK (krw >= 0);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'wallets'::regclass
          AND conname = 'ck_wallets_quantity_non_negative'
    ) THEN
        ALTER TABLE wallets
            ADD CONSTRAINT ck_wallets_quantity_non_negative CHECK (quantity >= 0);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'wallets'::regclass
          AND conname = 'ck_wallets_available_balance_non_negative'
    ) THEN
        ALTER TABLE wallets
            ADD CONSTRAINT ck_wallets_available_balance_non_negative CHECK (available_balance >= 0);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'wallets'::regclass
          AND conname = 'ck_wallets_locked_balance_non_negative'
    ) THEN
        ALTER TABLE wallets
            ADD CONSTRAINT ck_wallets_locked_balance_non_negative CHECK (locked_balance >= 0);
    END IF;

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

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'ledger_entries'::regclass
          AND conname = 'ck_ledger_entries_entry_type_valid'
    ) THEN
        ALTER TABLE ledger_entries
            ADD CONSTRAINT ck_ledger_entries_entry_type_valid
            CHECK (entry_type IN ('DEV_FUND', 'ORDER_HOLD', 'ORDER_RELEASE', 'TRADE_SETTLEMENT'));
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'ledger_entries'::regclass
          AND conname = 'ck_ledger_entries_reference_type_valid'
    ) THEN
        ALTER TABLE ledger_entries
            ADD CONSTRAINT ck_ledger_entries_reference_type_valid
            CHECK (reference_type IN ('DEV_FUND', 'ORDER', 'TRADE'));
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'ledger_entries'::regclass
          AND conname = 'ck_ledger_entries_has_delta'
    ) THEN
        ALTER TABLE ledger_entries
            ADD CONSTRAINT ck_ledger_entries_has_delta
            CHECK (available_delta <> 0 OR locked_delta <> 0);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'ledger_entries'::regclass
          AND conname = 'ck_ledger_entries_available_after_non_negative'
    ) THEN
        ALTER TABLE ledger_entries
            ADD CONSTRAINT ck_ledger_entries_available_after_non_negative CHECK (available_balance_after >= 0);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'ledger_entries'::regclass
          AND conname = 'ck_ledger_entries_locked_after_non_negative'
    ) THEN
        ALTER TABLE ledger_entries
            ADD CONSTRAINT ck_ledger_entries_locked_after_non_negative CHECK (locked_balance_after >= 0);
    END IF;
END $$;
