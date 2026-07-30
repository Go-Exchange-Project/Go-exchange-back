-- +goose Up
-- dependency 차단으로 생성되는 defer record는 실행 시도가 0회이므로 retry_count = 0에서
-- 시작해야 한다. 1에서 시작하면 SettlementRetryWorker의 RetryCount >= MaxRetryCount 검사에
-- 걸려 실제 시도 기회가 하나 줄어든다 — "차단은 retry budget을 소비하지 않는다"는 계약이
-- 저장소 계층에서 깨진다.
-- gorm AutoMigrate는 기존 CHECK를 갱신하지 않으므로 여기서 명시적으로 적용한다.

ALTER TABLE failed_market_completions
    ALTER COLUMN retry_count SET DEFAULT 0;

ALTER TABLE failed_market_completions
    DROP CONSTRAINT IF EXISTS ck_failed_market_completions_retry_count_positive;

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'failed_market_completions'::regclass
          AND conname = 'ck_failed_market_completions_retry_count_non_negative'
    ) THEN
        ALTER TABLE failed_market_completions
            ADD CONSTRAINT ck_failed_market_completions_retry_count_non_negative
            CHECK (retry_count >= 0);
    END IF;
END $$;
-- +goose StatementEnd

-- failed_order_cancellations 테이블 자체는 AutoMigrate가 만든다(001의 방침과 동일).
-- goose를 앱 부팅 경로 밖에서(AutoMigrate 없이) 단독 실행하는 운영 시나리오에
-- 대비해 테이블 존재를 가드한다.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_class WHERE relname = 'failed_order_cancellations') THEN
        ALTER TABLE failed_order_cancellations
            ALTER COLUMN retry_count SET DEFAULT 0;
    END IF;
END $$;
-- +goose StatementEnd

-- +goose Down
-- no-op: retry_count = 0인 행이 존재하면 CHECK를 다시 좁힐 수 없다.
-- 001_constraints.sql의 방침대로 안전한 Down만 제공한다.
SELECT 1;
