-- +goose NO TRANSACTION

-- 시장가 매수 완료(CompleteMarketOrder)는 매번 buyer_fee 합계를 조회한다.
-- trades.buy_order_id를 선두로 갖는 인덱스가 없어 체결이 쌓일수록 전수 스캔이 된다
-- (33번 진단: full 규모에서 이 쿼리 하나가 DB 실행시간의 86%, 호출 1건당 읽은 튜플이
-- 테이블 행 수와 사실상 같음).
--
-- 운영 테이블이므로 CONCURRENTLY로 만든다. CONCURRENTLY는 트랜잭션 블록 안에서
-- 돌 수 없어 NO TRANSACTION이 필요하고, 중단되면 같은 이름의 indisvalid=false
-- 인덱스를 남긴다. IF NOT EXISTS는 그 잔해를 "이미 있음"으로 보고 조용히 성공하므로,
-- 아래 카탈로그 검증이 같은 Up 안에 있어야 goose version이 잘못 기록되지 않는다.

-- +goose Up
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_trades_buy_order_id
    ON trades (buy_order_id);

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
        JOIN pg_attribute column_meta
          ON column_meta.attrelid = table_rel.oid
         AND column_meta.attnum = index_meta.indkey[0]
        WHERE index_ns.nspname = current_schema()
          AND table_ns.nspname = current_schema()
          AND table_rel.relname = 'trades'
          AND index_rel.relname = 'idx_trades_buy_order_id'
          AND access_method.amname = 'btree'
          AND column_meta.attname = 'buy_order_id'
          AND index_meta.indnkeyatts = 1
          AND index_meta.indnatts = 1
          AND index_meta.indisready
          AND index_meta.indisvalid
          AND NOT index_meta.indisunique
          AND index_meta.indexprs IS NULL
          AND index_meta.indpred IS NULL
    ) THEN
        RAISE EXCEPTION 'idx_trades_buy_order_id is missing, invalid, or has the wrong definition';
    END IF;
END $$;
-- +goose StatementEnd

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS idx_trades_buy_order_id;
