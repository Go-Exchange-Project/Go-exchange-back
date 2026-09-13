-- +goose Up
-- 단식 지갑·원장을 버린다. 옛 데이터를 새 표로 옮기지 않는다(설계 §10) —
-- 잔액은 이제 postings의 합이고, account_balances가 그 캐시다.
--
-- AutoMigrate 목록(cmd/main.go와 internal/testdb/integration.go)에서도 두 모델을
-- 함께 뺐다. 목록에 남겨 두면 다음 기동에서 AutoMigrate가 표를 다시 만든다.
DROP TABLE IF EXISTS wallets, ledger_entries CASCADE;

-- +goose Down
-- 되돌리지 않는다. 표를 다시 만들어도 그 안의 잔액은 복원되지 않으므로,
-- 빈 지갑 표는 있는 것이 없는 것보다 위험하다.
SELECT 1;
