-- +goose Up
-- Store the fee policy snapshot applied to each settled trade.

ALTER TABLE trades
    ADD COLUMN IF NOT EXISTS fee_rate numeric NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS buyer_fee numeric NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS buyer_fee_asset varchar(32) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS seller_fee numeric NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS seller_fee_asset varchar(32) NOT NULL DEFAULT '';

ALTER TABLE trades
    ALTER COLUMN fee_rate SET NOT NULL,
    ALTER COLUMN buyer_fee SET NOT NULL,
    ALTER COLUMN buyer_fee_asset SET NOT NULL,
    ALTER COLUMN seller_fee SET NOT NULL,
    ALTER COLUMN seller_fee_asset SET NOT NULL;

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'trades'::regclass
          AND conname = 'ck_trades_fee_rate_non_negative'
    ) THEN
        ALTER TABLE trades
            ADD CONSTRAINT ck_trades_fee_rate_non_negative CHECK (fee_rate >= 0);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'trades'::regclass
          AND conname = 'ck_trades_buyer_fee_non_negative'
    ) THEN
        ALTER TABLE trades
            ADD CONSTRAINT ck_trades_buyer_fee_non_negative CHECK (buyer_fee >= 0);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'trades'::regclass
          AND conname = 'ck_trades_seller_fee_non_negative'
    ) THEN
        ALTER TABLE trades
            ADD CONSTRAINT ck_trades_seller_fee_non_negative CHECK (seller_fee >= 0);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'trades'::regclass
          AND conname = 'ck_trades_buyer_fee_asset_when_fee_positive'
    ) THEN
        ALTER TABLE trades
            ADD CONSTRAINT ck_trades_buyer_fee_asset_when_fee_positive
            CHECK (buyer_fee = 0 OR length(btrim(buyer_fee_asset)) > 0);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'trades'::regclass
          AND conname = 'ck_trades_seller_fee_asset_when_fee_positive'
    ) THEN
        ALTER TABLE trades
            ADD CONSTRAINT ck_trades_seller_fee_asset_when_fee_positive
            CHECK (seller_fee = 0 OR length(btrim(seller_fee_asset)) > 0);
    END IF;
END $$;
-- +goose StatementEnd

-- +goose Down
ALTER TABLE trades
    DROP CONSTRAINT IF EXISTS ck_trades_seller_fee_asset_when_fee_positive,
    DROP CONSTRAINT IF EXISTS ck_trades_buyer_fee_asset_when_fee_positive,
    DROP CONSTRAINT IF EXISTS ck_trades_seller_fee_non_negative,
    DROP CONSTRAINT IF EXISTS ck_trades_buyer_fee_non_negative,
    DROP CONSTRAINT IF EXISTS ck_trades_fee_rate_non_negative;

ALTER TABLE trades
    DROP COLUMN IF EXISTS seller_fee_asset,
    DROP COLUMN IF EXISTS seller_fee,
    DROP COLUMN IF EXISTS buyer_fee_asset,
    DROP COLUMN IF EXISTS buyer_fee,
    DROP COLUMN IF EXISTS fee_rate;
