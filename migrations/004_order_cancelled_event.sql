-- +goose Up
-- Widen the trade_outbox_events event_type CHECK to allow ORDER_CANCELLED.
-- gorm AutoMigrate does not alter existing CHECK constraints, so the widened
-- constraint must be applied explicitly here regardless of whether the table
-- was just created by AutoMigrate (fresh DB, old CHECK) or already existed.

ALTER TABLE trade_outbox_events
    DROP CONSTRAINT IF EXISTS ck_trade_outbox_event_type;

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'trade_outbox_events'::regclass
          AND conname = 'ck_trade_outbox_event_type'
    ) THEN
        ALTER TABLE trade_outbox_events
            ADD CONSTRAINT ck_trade_outbox_event_type
            CHECK (event_type IN ('TRADE', 'MARKET_ORDER_DONE', 'ORDER_CANCELLED'));
    END IF;
END $$;
-- +goose StatementEnd

-- +goose Down
ALTER TABLE trade_outbox_events
    DROP CONSTRAINT IF EXISTS ck_trade_outbox_event_type;

ALTER TABLE trade_outbox_events
    ADD CONSTRAINT ck_trade_outbox_event_type
    CHECK (event_type IN ('TRADE', 'MARKET_ORDER_DONE'));
