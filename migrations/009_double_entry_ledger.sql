-- +goose Up
-- 복식부기 원장 7개 표의 제약. 표 자체는 AutoMigrate가 만들고(001과 같은 방식),
-- 여기서는 GORM 태그로 표현할 수 없는 것만 건다.
--
-- 대상: accounts, account_balances, journal_entries, postings,
--       transfer_requests, transfer_status_events, user_asset_stats

-- ---------------------------------------------------------------------------
-- accounts
-- ---------------------------------------------------------------------------

-- 계정은 (종류, 소유자, 자산)으로 유일하다. 시스템 계정은 owner_user_id가 NULL이고
-- Postgres에서 NULL은 서로 다르게 취급되므로, 일반 UNIQUE로는 FEE_INCOME KRW 계정이
-- 여러 개 생길 수 있다. COALESCE로 NULL을 0으로 접어 그것을 막는다.
CREATE UNIQUE INDEX IF NOT EXISTS accounts_type_owner_asset_unique
    ON accounts (account_type, COALESCE(owner_user_id, 0), asset);

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'accounts'::regclass AND conname = 'accounts_type_check'
    ) THEN
        ALTER TABLE accounts ADD CONSTRAINT accounts_type_check
            CHECK (account_type IN (
                'USER_AVAILABLE','USER_LOCKED','FEE_INCOME',
                'EXTERNAL_BANK','EXTERNAL_CHAIN','DEV_MINT'));
    END IF;

    -- 사용자 계정은 소유자가 있어야 하고, 시스템 계정은 없어야 한다. 뒤집히면
    -- "누구의 돈인지 모르는 사용자 잔액"이나 "특정 사용자에게 귀속된 수수료 금고"가 생긴다.
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'accounts'::regclass AND conname = 'accounts_owner_presence_check'
    ) THEN
        ALTER TABLE accounts ADD CONSTRAINT accounts_owner_presence_check
            CHECK (
                (account_type IN ('USER_AVAILABLE','USER_LOCKED') AND owner_user_id IS NOT NULL)
                OR
                (account_type IN ('FEE_INCOME','EXTERNAL_BANK','EXTERNAL_CHAIN','DEV_MINT') AND owner_user_id IS NULL)
            );
    END IF;
END $$;
-- +goose StatementEnd

-- ---------------------------------------------------------------------------
-- account_balances
-- ---------------------------------------------------------------------------

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'account_balances'::regclass AND conname = 'account_balances_account_fk'
    ) THEN
        ALTER TABLE account_balances ADD CONSTRAINT account_balances_account_fk
            FOREIGN KEY (account_id) REFERENCES accounts (id);
    END IF;
END $$;
-- +goose StatementEnd

-- ---------------------------------------------------------------------------
-- journal_entries
-- ---------------------------------------------------------------------------

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'journal_entries'::regclass AND conname = 'journal_entries_event_type_check'
    ) THEN
        ALTER TABLE journal_entries ADD CONSTRAINT journal_entries_event_type_check
            CHECK (event_type IN (
                'DEPOSIT','WITHDRAWAL','ORDER_HOLD','ORDER_RELEASE',
                'TRADE','DEV_FUND','REVERSAL'));
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'journal_entries'::regclass AND conname = 'journal_entries_reference_type_check'
    ) THEN
        ALTER TABLE journal_entries ADD CONSTRAINT journal_entries_reference_type_check
            CHECK (reference_type IN ('ORDER','TRADE','TRANSFER','DEV_FUND'));
    END IF;

    -- 역분개일 때만 원본을 가리킨다. 한쪽만 채워지면 "무엇을 되돌렸는지 모르는
    -- 역분개"나 "역분개가 아닌데 원본을 가리키는 분개"가 된다.
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'journal_entries'::regclass AND conname = 'journal_entries_reversal_pair_check'
    ) THEN
        ALTER TABLE journal_entries ADD CONSTRAINT journal_entries_reversal_pair_check
            CHECK ((event_type = 'REVERSAL') = (reverses_journal_id IS NOT NULL));
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'journal_entries'::regclass AND conname = 'journal_entries_reverses_fk'
    ) THEN
        ALTER TABLE journal_entries ADD CONSTRAINT journal_entries_reverses_fk
            FOREIGN KEY (reverses_journal_id) REFERENCES journal_entries (id);
    END IF;
END $$;
-- +goose StatementEnd

-- ---------------------------------------------------------------------------
-- postings
-- ---------------------------------------------------------------------------

-- 계정별 합계용. id를 뒤에 붙여 "이 전기까지의 합"을 인덱스만으로 낼 수 있게 한다.
CREATE INDEX IF NOT EXISTS postings_account_id_id_idx ON postings (account_id, id);
CREATE INDEX IF NOT EXISTS postings_journal_id_idx ON postings (journal_id);

-- +goose StatementBegin
DO $$
BEGIN
    -- 금액 0짜리 전기는 아무 사실도 기록하지 않으면서 분개만 늘린다.
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'postings'::regclass AND conname = 'postings_amount_nonzero_check'
    ) THEN
        ALTER TABLE postings ADD CONSTRAINT postings_amount_nonzero_check
            CHECK (amount <> 0);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'postings'::regclass AND conname = 'postings_journal_fk'
    ) THEN
        ALTER TABLE postings ADD CONSTRAINT postings_journal_fk
            FOREIGN KEY (journal_id) REFERENCES journal_entries (id);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'postings'::regclass AND conname = 'postings_account_fk'
    ) THEN
        ALTER TABLE postings ADD CONSTRAINT postings_account_fk
            FOREIGN KEY (account_id) REFERENCES accounts (id);
    END IF;
END $$;
-- +goose StatementEnd

-- ---------------------------------------------------------------------------
-- transfer_requests
-- ---------------------------------------------------------------------------

-- external_ref는 외부 제출 전(RECEIVED)에 NULL이다. 부분 유니크 인덱스라야
-- 접수만 된 요청 여러 건이 NULL을 공유할 수 있다.
CREATE UNIQUE INDEX IF NOT EXISTS transfer_requests_external_ref_unique
    ON transfer_requests (external_ref) WHERE external_ref IS NOT NULL;

-- 조회 작업이 대상을 고르는 인덱스. 정상 상태에서는 거의 비어 있다.
CREATE INDEX IF NOT EXISTS transfer_requests_next_check_at_idx
    ON transfer_requests (next_check_at) WHERE status = 'PROCESSING';

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'transfer_requests'::regclass AND conname = 'transfer_requests_enum_check'
    ) THEN
        ALTER TABLE transfer_requests ADD CONSTRAINT transfer_requests_enum_check
            CHECK (direction IN ('DEPOSIT','WITHDRAWAL')
               AND rail IN ('BANK','CHAIN')
               AND status IN ('RECEIVED','PROCESSING','COMPLETED','FAILED'));
    END IF;

    -- 서버도 같은 것을 막지만, 검증을 우회하는 경로가 생겨도 DB가 막는다.
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'transfer_requests'::regclass AND conname = 'transfer_requests_amount_check'
    ) THEN
        ALTER TABLE transfer_requests ADD CONSTRAINT transfer_requests_amount_check
            CHECK (amount > 0 AND fee_amount >= 0);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'transfer_requests'::regclass AND conname = 'transfer_requests_bank_asset_check'
    ) THEN
        ALTER TABLE transfer_requests ADD CONSTRAINT transfer_requests_bank_asset_check
            CHECK (rail <> 'BANK' OR asset = 'KRW');
    END IF;

    -- 입금은 잠글 것이 없다. 출금 쪽 방향(WITHDRAWAL이면 반드시 있다)은 즉시
    -- CHECK로 걸 수 없어 아래 constraint trigger가 맡는다.
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'transfer_requests'::regclass AND conname = 'transfer_requests_deposit_no_hold_check'
    ) THEN
        ALTER TABLE transfer_requests ADD CONSTRAINT transfer_requests_deposit_no_hold_check
            CHECK (direction <> 'DEPOSIT' OR hold_journal_id IS NULL);
    END IF;

    -- 완료됐으면 그것을 만든 분개가 있어야 한다. 출금 실패도 잠근 돈을 푼
    -- 분개가 있어야 한다 — 입금 실패는 분개가 없으므로 direction으로 가른다.
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'transfer_requests'::regclass AND conname = 'transfer_requests_resolution_journal_check'
    ) THEN
        ALTER TABLE transfer_requests ADD CONSTRAINT transfer_requests_resolution_journal_check
            CHECK (
                (status <> 'COMPLETED' OR resolution_journal_id IS NOT NULL)
                AND
                (status <> 'FAILED' OR direction <> 'WITHDRAWAL' OR resolution_journal_id IS NOT NULL)
            );
    END IF;

    -- 외부에 제출한 뒤에는 외부 거래번호가 반드시 있다.
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'transfer_requests'::regclass AND conname = 'transfer_requests_external_ref_presence_check'
    ) THEN
        ALTER TABLE transfer_requests ADD CONSTRAINT transfer_requests_external_ref_presence_check
            CHECK (status = 'RECEIVED' OR external_ref IS NOT NULL);
    END IF;

    -- 운영 확인 표시는 두 열이 함께 움직인다. 사유 없는 깃발은 운영자가
    -- 무엇을 봐야 할지 알 수 없고, 깃발 없는 사유는 아무도 보지 않는다.
    --
    -- NULL 비교로 쓰면 안 된다: review_reason이 NULL일 때 (review_reason = '')는
    -- FALSE가 아니라 NULL이 되고, CHECK는 FALSE일 때만 거부하므로 잘못된 조합이
    -- 그대로 통과한다. 허용 조합을 직접 나열한다.
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'transfer_requests'::regclass AND conname = 'transfer_requests_review_pair_check'
    ) THEN
        ALTER TABLE transfer_requests ADD CONSTRAINT transfer_requests_review_pair_check
            CHECK (
                (review_required_at IS NULL AND review_reason IS NULL)
                OR
                (review_required_at IS NOT NULL AND review_reason IS NOT NULL AND review_reason <> '')
            );
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'transfer_requests'::regclass AND conname = 'transfer_requests_hold_journal_fk'
    ) THEN
        ALTER TABLE transfer_requests ADD CONSTRAINT transfer_requests_hold_journal_fk
            FOREIGN KEY (hold_journal_id) REFERENCES journal_entries (id);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'transfer_requests'::regclass AND conname = 'transfer_requests_resolution_journal_fk'
    ) THEN
        ALTER TABLE transfer_requests ADD CONSTRAINT transfer_requests_resolution_journal_fk
            FOREIGN KEY (resolution_journal_id) REFERENCES journal_entries (id);
    END IF;
END $$;
-- +goose StatementEnd

-- 출금은 반드시 잠금 분개를 가진다. 이것을 즉시 CHECK로 걸면 출금 요청을
-- 만들 수 없다 — 순환이 생기기 때문이다:
--
--   잠금 분개를 만들려면 → reference_id로 쓸 요청 id가 필요하다
--   요청 id를 얻으려면   → 요청을 INSERT해야 한다
--   요청을 INSERT하려면  → hold_journal_id가 이미 있어야 한다   ← 즉시 CHECK
--
-- 요청 INSERT를 먼저 하는 것은 선택이 아니다. client_request_key를 선점해야
-- 같은 요청이 두 번 잠그지 않는다. 그래서 커밋 시점의 최종 행만 보는
-- DEFERRABLE constraint trigger로 검사한다.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION transfer_requests_require_withdrawal_hold() RETURNS trigger AS $$
DECLARE
    final_row transfer_requests%ROWTYPE;
BEGIN
    -- 지연 트리거의 NEW는 **사건이 일어난 시점**의 값이지 커밋 시점의 값이 아니다.
    -- INSERT 사건의 NEW는 hold_journal_id가 NULL이므로, NEW를 그대로 보면 나중에
    -- 채우는 정상 흐름까지 커밋에서 터진다. 최종 행을 다시 읽어야 한다.
    SELECT * INTO final_row FROM transfer_requests WHERE id = NEW.id;
    IF NOT FOUND THEN
        -- 같은 트랜잭션에서 지워졌다. 검사할 행이 없다.
        RETURN NULL;
    END IF;

    IF final_row.direction = 'WITHDRAWAL' AND final_row.hold_journal_id IS NULL THEN
        RAISE EXCEPTION 'withdrawal transfer_request % has no hold_journal_id at commit', final_row.id;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_trigger
        WHERE tgrelid = 'transfer_requests'::regclass
          AND tgname = 'transfer_requests_withdrawal_hold_trigger'
          AND NOT tgisinternal
    ) THEN
        CREATE CONSTRAINT TRIGGER transfer_requests_withdrawal_hold_trigger
            AFTER INSERT OR UPDATE ON transfer_requests
            DEFERRABLE INITIALLY DEFERRED
            FOR EACH ROW
            EXECUTE FUNCTION transfer_requests_require_withdrawal_hold();
    END IF;
END $$;
-- +goose StatementEnd

-- ---------------------------------------------------------------------------
-- transfer_status_events
-- ---------------------------------------------------------------------------

-- 운영자가 한 요청의 사건을 시간순으로 본다.
CREATE INDEX IF NOT EXISTS transfer_status_events_request_received_idx
    ON transfer_status_events (transfer_request_id, received_at);

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'transfer_status_events'::regclass AND conname = 'transfer_status_events_enum_check'
    ) THEN
        ALTER TABLE transfer_status_events ADD CONSTRAINT transfer_status_events_enum_check
            CHECK (source IN ('CALLBACK','POLL')
               AND outcome IN ('SUCCESS','FAILURE','PENDING','UNKNOWN'));
    END IF;

    -- 대상 없는 사건이 들어오는 것을 DB가 막는다. 우리가 모르는 요청의 알림을
    -- 저장할 이유가 없다.
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'transfer_status_events'::regclass AND conname = 'transfer_status_events_request_fk'
    ) THEN
        ALTER TABLE transfer_status_events ADD CONSTRAINT transfer_status_events_request_fk
            FOREIGN KEY (transfer_request_id) REFERENCES transfer_requests (id);
    END IF;
END $$;
-- +goose StatementEnd

-- ---------------------------------------------------------------------------
-- user_asset_stats
-- ---------------------------------------------------------------------------

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'user_asset_stats'::regclass AND conname = 'user_asset_stats_avg_buy_price_check'
    ) THEN
        ALTER TABLE user_asset_stats ADD CONSTRAINT user_asset_stats_avg_buy_price_check
            CHECK (avg_buy_price >= 0);
    END IF;
END $$;
-- +goose StatementEnd

-- +goose Down
-- 원장은 data-bearing이므로 자동으로 지우지 않는다. rollback이 필요하면
-- 별도 운영 절차에서 처리한다(008과 같은 방침).
SELECT 1;
