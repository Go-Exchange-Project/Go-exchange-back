-- +goose Up
-- 주문 생성 재시도를 식별하는 키. 스키마는 이 migration이 단독으로 소유한다
-- (AutoMigrate에 넣으면 GORM이 아래 UNIQUE를 자기 명명규칙으로 DROP하려 한다).

CREATE TABLE IF NOT EXISTS order_idempotency_keys (
    id                  BIGSERIAL PRIMARY KEY,
    user_id             BIGINT      NOT NULL,
    idempotency_key     TEXT        NOT NULL,
    fingerprint         TEXT        NOT NULL,
    fingerprint_version INT         NOT NULL,
    order_id            BIGINT,
    outcome             TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'order_idempotency_keys'::regclass
          AND conname = 'order_idempotency_keys_user_key_unique'
    ) THEN
        ALTER TABLE order_idempotency_keys
            ADD CONSTRAINT order_idempotency_keys_user_key_unique UNIQUE (user_id, idempotency_key);
    END IF;

    -- HTTP 계약(공백 제외 1~128자)과 같은 범위를 DB에서도 막는다.
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'order_idempotency_keys'::regclass
          AND conname = 'order_idempotency_keys_key_length'
    ) THEN
        ALTER TABLE order_idempotency_keys
            ADD CONSTRAINT order_idempotency_keys_key_length
            CHECK (length(btrim(idempotency_key)) BETWEEN 1 AND 128);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'order_idempotency_keys'::regclass
          AND conname = 'order_idempotency_keys_outcome_check'
    ) THEN
        ALTER TABLE order_idempotency_keys
            ADD CONSTRAINT order_idempotency_keys_outcome_check
            CHECK (outcome IS NULL OR outcome IN ('PENDING','ACCEPTED','REJECTED','UNKNOWN'));
    END IF;
END $$;
-- +goose StatementEnd

-- stale PENDING gauge 조회 전용. 정상 상태에서는 거의 비어 있다.
CREATE INDEX IF NOT EXISTS order_idempotency_pending_updated_at
    ON order_idempotency_keys (updated_at)
    WHERE outcome = 'PENDING';

-- IF NOT EXISTS는 같은 이름의 잘못된 인덱스도 조용히 통과시킨다(006에서 확인).
-- 카탈로그로 확인하고 어긋나면 실패시켜 goose version 8이 기록되지 않게 한다.
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_class index_rel
        JOIN pg_index index_meta ON index_meta.indexrelid = index_rel.oid
        JOIN pg_am access_method ON access_method.oid = index_rel.relam
        JOIN pg_attribute column_meta
          ON column_meta.attrelid = index_meta.indrelid
         AND column_meta.attnum = index_meta.indkey[0]
        WHERE index_rel.relname = 'order_idempotency_pending_updated_at'
          AND index_meta.indrelid = 'order_idempotency_keys'::regclass
          AND index_meta.indisready
          AND index_meta.indisvalid
          AND NOT index_meta.indisunique
          AND access_method.amname = 'btree'
          AND index_meta.indnkeyatts = 1
          AND index_meta.indnatts = 1
          AND column_meta.attname = 'updated_at'
          AND index_meta.indexprs IS NULL
          AND pg_get_expr(index_meta.indpred, index_meta.indrelid) = '(outcome = ''PENDING''::text)'
    ) THEN
        RAISE EXCEPTION 'order_idempotency_pending_updated_at is missing, invalid, or has the wrong definition';
    END IF;
END $$;
-- +goose StatementEnd

-- +goose Down
-- data-bearing 키를 자동으로 지우지 않는다. rollback이 필요하면 별도 운영 절차에서 처리한다.
SELECT 1;
