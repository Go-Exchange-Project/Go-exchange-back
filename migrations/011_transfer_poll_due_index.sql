-- +goose NO TRANSACTION

-- DueForCheck(internal/repository/transfer_repository.go)는
--   WHERE status IN ('RECEIVED','PROCESSING')
--     AND (next_check_at IS NULL OR next_check_at <= $now)
--   ORDER BY next_check_at ASC NULLS FIRST, id ASC
--   LIMIT $batch
-- 를 5초마다 돌린다. 009의 transfer_requests_next_check_at_idx는 PROCESSING만
-- 포함하고 키도 next_check_at 하나라 이 조회를 받치지 못한다 — RECEIVED가
-- 빠지고, 정렬 키에 id가 없어 동시각 다건에서 Sort가 붙는다. 완료된 입출금
-- 이력이 쌓이면 이 조회가 큰 표를 스캔·정렬한다.
--
-- 운영 테이블이므로 CONCURRENTLY로 만든다. CONCURRENTLY는 트랜잭션 블록 안에서
-- 돌 수 없어 NO TRANSACTION이 필요하고, 중단되면 같은 이름의 indisvalid=false
-- 인덱스를 남긴다. IF NOT EXISTS는 그 잔해를 "이미 있음"으로 보고 조용히
-- 성공하므로, 아래 카탈로그 검증이 같은 Up 안에 있어야 goose version이 잘못
-- 기록되지 않는다(006 주석 참조).

-- +goose Up
CREATE INDEX CONCURRENTLY IF NOT EXISTS transfer_requests_due_poll_idx
    ON transfer_requests (next_check_at ASC NULLS FIRST, id ASC)
    WHERE status IN ('RECEIVED', 'PROCESSING');

-- 기대 문자열은 PostgreSQL 16.15와 18.6의 pg_get_expr 출력이 동일함을 확인했다.
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_class index_rel
        JOIN pg_namespace index_ns ON index_ns.oid = index_rel.relnamespace
        JOIN pg_index index_meta ON index_meta.indexrelid = index_rel.oid
        JOIN pg_class table_rel ON table_rel.oid = index_meta.indrelid
        JOIN pg_namespace table_ns ON table_ns.oid = table_rel.relnamespace
        JOIN pg_am access_method ON access_method.oid = index_rel.relam
        JOIN pg_attribute first_column
          ON first_column.attrelid = table_rel.oid
         AND first_column.attnum = index_meta.indkey[0]
        JOIN pg_attribute second_column
          ON second_column.attrelid = table_rel.oid
         AND second_column.attnum = index_meta.indkey[1]
        WHERE index_ns.nspname = current_schema()
          AND table_ns.nspname = current_schema()
          AND table_rel.relname = 'transfer_requests'
          AND index_rel.relname = 'transfer_requests_due_poll_idx'
          AND access_method.amname = 'btree'
          AND index_meta.indisready
          AND index_meta.indisvalid
          AND NOT index_meta.indisunique
          AND index_meta.indnkeyatts = 2
          AND index_meta.indnatts = 2
          AND first_column.attname = 'next_check_at'
          -- indoption 비트: 0=DESC, 1=NULLS FIRST. ASC NULLS FIRST는 2(비트 1만).
          -- PostgreSQL의 ASC 기본값은 NULLS LAST이므로, 이 비트가 없으면 인덱스가
          -- ORDER BY next_check_at ASC NULLS FIRST 순서를 만들지 못해 Sort가 붙는다.
          AND index_meta.indoption[0] = 2
          AND second_column.attname = 'id'
          AND index_meta.indoption[1] = 0
          AND index_meta.indexprs IS NULL
          AND pg_get_expr(index_meta.indpred, index_meta.indrelid)
              = $pred$((status)::text = ANY ((ARRAY['RECEIVED'::character varying, 'PROCESSING'::character varying])::text[]))$pred$
    ) THEN
        RAISE EXCEPTION 'transfer_requests_due_poll_idx is missing, invalid, or has the wrong definition';
    END IF;
END $$;
-- +goose StatementEnd

-- 검증이 끝난 뒤에만 옛 인덱스를 지운다. 이 조회를 새 인덱스가 받치므로,
-- 옛 인덱스를 남기면 조회에 쓰이지 않으면서 쓰기 비용만 남는다.
DROP INDEX CONCURRENTLY IF EXISTS transfer_requests_next_check_at_idx;

-- Up과 같은 규율이다: 옛 인덱스 생성이 중단되면 같은 이름의 indisvalid=false
-- 잔해가 남고, IF NOT EXISTS는 그 잔해를 "이미 있음"으로 보고 재생성을
-- 건너뛴다. 재시도 시 검증 없이 새 인덱스를 지우면 goose는 Down 완료로
-- 기록하는데 유효한 조회 인덱스가 하나도 남지 않는다 — Up에서 막은 것과 같은
-- 실패 유형이다. 그래서 옛 인덱스를 검증한 뒤에만 새 인덱스를 지운다.
--
-- 복구 절차: 이 검증에서 실패하면 invalid한 transfer_requests_next_check_at_idx를
-- DROP INDEX CONCURRENTLY로 지운 뒤 Down을 다시 실행한다. 그동안
-- transfer_requests_due_poll_idx는 남아 있다.
-- +goose Down
CREATE INDEX CONCURRENTLY IF NOT EXISTS transfer_requests_next_check_at_idx
    ON transfer_requests (next_check_at) WHERE status = 'PROCESSING';

-- 기대 문자열은 PostgreSQL 16.15와 18.6의 pg_get_expr 출력이 동일함을 확인했다.
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_class index_rel
        JOIN pg_namespace index_ns ON index_ns.oid = index_rel.relnamespace
        JOIN pg_index index_meta ON index_meta.indexrelid = index_rel.oid
        JOIN pg_class table_rel ON table_rel.oid = index_meta.indrelid
        JOIN pg_namespace table_ns ON table_ns.oid = table_rel.relnamespace
        JOIN pg_am access_method ON access_method.oid = index_rel.relam
        JOIN pg_attribute first_column
          ON first_column.attrelid = table_rel.oid
         AND first_column.attnum = index_meta.indkey[0]
        WHERE index_ns.nspname = current_schema()
          AND table_ns.nspname = current_schema()
          AND table_rel.relname = 'transfer_requests'
          AND index_rel.relname = 'transfer_requests_next_check_at_idx'
          AND access_method.amname = 'btree'
          AND index_meta.indisready
          AND index_meta.indisvalid
          AND NOT index_meta.indisunique
          AND index_meta.indnkeyatts = 1
          AND index_meta.indnatts = 1
          AND first_column.attname = 'next_check_at'
          -- indoption 비트 0=DESC, 1=NULLS FIRST. 009가 기본값(ASC NULLS LAST)으로
          -- 만들었으므로 0이어야 한다 — 011의 새 인덱스(2, ASC NULLS FIRST)와 다르다.
          AND index_meta.indoption[0] = 0
          AND index_meta.indexprs IS NULL
          AND pg_get_expr(index_meta.indpred, index_meta.indrelid)
              = $pred$((status)::text = 'PROCESSING'::text)$pred$
    ) THEN
        RAISE EXCEPTION 'transfer_requests_next_check_at_idx is missing, invalid, or has the wrong definition';
    END IF;
END $$;
-- +goose StatementEnd

DROP INDEX CONCURRENTLY IF EXISTS transfer_requests_due_poll_idx;
