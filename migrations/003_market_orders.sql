-- +goose Up
-- Add quote-side accounting fields required for market buy orders.

ALTER TABLE orders
    ADD COLUMN IF NOT EXISTS quote_amount numeric NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS filled_quote_amount numeric NOT NULL DEFAULT 0;

-- The old baseline only allowed amount > 0. Market buy orders use quote_amount
-- instead, so amount may be 0 for that order shape.
ALTER TABLE orders
    DROP CONSTRAINT IF EXISTS ck_orders_amount_positive;

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'orders'::regclass
          AND conname = 'ck_orders_amount_non_negative'
    ) THEN
        ALTER TABLE orders
            ADD CONSTRAINT ck_orders_amount_non_negative CHECK (amount >= 0);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'orders'::regclass
          AND conname = 'ck_orders_quote_amount_non_negative'
    ) THEN
        ALTER TABLE orders
            ADD CONSTRAINT ck_orders_quote_amount_non_negative CHECK (quote_amount >= 0);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'orders'::regclass
          AND conname = 'ck_orders_filled_quote_amount_non_negative'
    ) THEN
        ALTER TABLE orders
            ADD CONSTRAINT ck_orders_filled_quote_amount_non_negative CHECK (filled_quote_amount >= 0);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'orders'::regclass
          AND conname = 'ck_orders_shape_by_type'
    ) THEN
        ALTER TABLE orders
            ADD CONSTRAINT ck_orders_shape_by_type CHECK (
                (
                    order_type = 'LIMIT'
                    AND price > 0
                    AND amount > 0
                    AND quote_amount = 0
                )
                OR (
                    order_type = 'MARKET'
                    AND side = 'BUY'
                    AND price = 0
                    AND amount = 0
                    AND quote_amount > 0
                )
                OR (
                    order_type = 'MARKET'
                    AND side = 'SELL'
                    AND price = 0
                    AND amount > 0
                    AND quote_amount = 0
                )
            );
    END IF;
END $$;
-- +goose StatementEnd

-- +goose Down
ALTER TABLE orders
    DROP CONSTRAINT IF EXISTS ck_orders_shape_by_type,
    DROP CONSTRAINT IF EXISTS ck_orders_filled_quote_amount_non_negative,
    DROP CONSTRAINT IF EXISTS ck_orders_quote_amount_non_negative,
    DROP CONSTRAINT IF EXISTS ck_orders_amount_non_negative;

ALTER TABLE orders
    ADD CONSTRAINT ck_orders_amount_positive CHECK (amount > 0);

ALTER TABLE orders
    DROP COLUMN IF EXISTS filled_quote_amount,
    DROP COLUMN IF EXISTS quote_amount;
