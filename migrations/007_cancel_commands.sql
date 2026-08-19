-- +goose Up
-- 취소 의도를 엔진에 넣기 전에 내구 기록하는 command 테이블이다. 사용자에게 접수를
-- 알린 뒤 프로세스가 죽어도 재기동한 worker가 이 행을 보고 취소를 다시 실행한다.
--
-- 앱은 AutoMigrate를 먼저 돌린 뒤 goose를 실행하므로 "테이블이 이미 있음"도 반드시
-- 통과해야 한다. 그래서 CREATE TABLE IF NOT EXISTS 뒤에 이름이 고정된 제약을 보강한다.

CREATE TABLE IF NOT EXISTS cancel_commands (
    id            BIGSERIAL PRIMARY KEY,
    order_id      BIGINT      NOT NULL,
    user_id       BIGINT      NOT NULL,
    coin_symbol   TEXT        NOT NULL,
    side          TEXT        NOT NULL,
    price         NUMERIC     NOT NULL,
    status        TEXT        NOT NULL DEFAULT 'PENDING',
    attempt_count INT         NOT NULL DEFAULT 0,
    last_error    TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- price가 없으면 worker가 matching.CancelOrderCommand를 복원할 수 없다. AutoMigrate가
-- 먼저 만든 컬럼이 nullable로 남아 있을 수 있으므로 여기서 명시적으로 고정한다.
ALTER TABLE cancel_commands
    ALTER COLUMN price TYPE NUMERIC,
    ALTER COLUMN price SET NOT NULL;

ALTER TABLE cancel_commands
    ALTER COLUMN status SET DEFAULT 'PENDING',
    ALTER COLUMN status SET NOT NULL;

ALTER TABLE cancel_commands
    ALTER COLUMN attempt_count SET DEFAULT 0,
    ALTER COLUMN attempt_count SET NOT NULL;

-- 주문은 한 번만 취소할 수 있다. 부분 UNIQUE(WHERE status='PENDING')로 만들면
-- command가 PROCESSED이고 정산은 아직 안 끝난 창에서 두 번째 command가 생겨
-- ORDER_RELEASE가 두 번 날 수 있다. 전체 UNIQUE여야 그 창이 없다.
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'cancel_commands'::regclass
          AND conname = 'cancel_commands_order_unique'
    ) THEN
        ALTER TABLE cancel_commands
            ADD CONSTRAINT cancel_commands_order_unique UNIQUE (order_id);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'cancel_commands'::regclass
          AND conname = 'cancel_commands_status_check'
    ) THEN
        ALTER TABLE cancel_commands
            ADD CONSTRAINT cancel_commands_status_check
            CHECK (status IN ('PENDING','PROCESSED','NOOP'));
    END IF;
END $$;
-- +goose StatementEnd

-- worker의 PENDING 스캔 전용이다. 이쪽만 부분 인덱스다.
CREATE INDEX IF NOT EXISTS cancel_commands_pending
    ON cancel_commands (id) WHERE status = 'PENDING';

-- +goose Down
-- data-bearing command를 자동으로 지우지 않는다. rollback이 필요하면 PENDING 0과
-- 백업을 먼저 확인하는 별도 운영 절차에서 처리한다.
SELECT 1;
