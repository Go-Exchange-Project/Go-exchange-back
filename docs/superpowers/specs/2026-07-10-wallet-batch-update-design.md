# 정산 지갑 갱신 배치화 설계

## 배경 (왜 필요한가)

`14-2026-07-10-pg-stat-statements-investigation.md`와 `15-2026-07-10-db-cpu-scaling-control.md`에서, Postgres CPU 포화의 원인이 정산 트랜잭션당 약 15회의 쿼리 왕복이라는 걸 확인했고, DB 인스턴스 CPU 증설로 이 문제를 해소했다. 하지만 사용자가 "계속 인프라 스케일업으로만 해결하는 것 같다"고 지적했고, 이제는 코드 레벨에서 왕복 횟수 자체를 줄이는 방향으로 진행하기로 했다.

`internal/service/settlement_service.go`의 `SettleTrade`를 보면, 지갑 갱신(`UPDATE`)이 4번 개별 호출된다(매수자 KRW, 매수자 코인, 매도자 코인, 매도자 KRW) — 각각 다른 행을 갱신하지만 개별 SQL 왕복이다. 이 4개를 하나의 다중 행 UPDATE로 묶으면, 정산 하나당 쿼리 왕복을 줄일 수 있다.

## 왜 이 방식을 선택했는지

07번(CPU 핀닝)에서 제안했던 4개 지점(주문 조회 2→1, 지갑 조회 4→1, 주문 갱신 2→1, 지갑 갱신 4→1) 중, **지갑 갱신(UPDATE) 4개를 먼저** 선택했다 — 가장 큰 이득(4→1)이면서도, `FOR UPDATE` 조회를 합치는 것보다 안전하다. 조회를 합치는 건 잠금 순서·행 매핑을 다시 설계해야 해서 리스크가 크지만, UPDATE 묶기는 "이미 계산된 최종 값을 어디에 쓸지"만 바뀌는 거라 정합성 리스크가 상대적으로 작다.

`settlement_service.go`를 보면 이미 4개 지갑 각각에 대해 `WalletBalanceUpdate`(최종 반영될 `available_balance`/`locked_balance`/`krw`/`quantity`/`avg_buy_price`가 다 계산된 구조체)를 갖고 있다 — 이 값들을 그대로 재사용해서 한 번의 SQL로 묶기만 하면 된다.

## 범위

- `internal/repository/wallet_reporsitory.go`에 `WalletBatchUpdate` 타입과 `BatchUpdateBalances` 메서드를 추가한다.
- `internal/service/settlement_service.go`에서 4번의 개별 지갑 UPDATE 호출을 `BatchUpdateBalances` 한 번으로 교체한다.
- 정산 결과(지갑 최종 상태)는 기존과 완전히 동일해야 한다 — 기존 정산 통합 테스트가 그대로 통과해야 한다.
- 주문 조회/갱신, 지갑 조회를 합치는 건 이번 스코프가 아니다 — 결과를 보고 필요하면 별도로 진행한다.

## 아키텍처

### 1. `internal/repository/wallet_reporsitory.go`에 배치 갱신 추가

```go
type WalletBatchUpdate struct {
	WalletID         uint
	AvailableBalance decimal.Decimal
	LockedBalance    decimal.Decimal
	KRW              decimal.Decimal
	Quantity         decimal.Decimal
	AvgBuyPrice      decimal.Decimal
}

func (r *WalletRepository) BatchUpdateBalances(updates []WalletBatchUpdate) error {
	if len(updates) == 0 {
		return nil
	}

	rows := make([]string, 0, len(updates))
	args := make([]interface{}, 0, len(updates)*6)
	for i, u := range updates {
		base := i * 6
		rows = append(rows, fmt.Sprintf(
			"($%d::bigint, $%d::numeric, $%d::numeric, $%d::numeric, $%d::numeric, $%d::numeric)",
			base+1, base+2, base+3, base+4, base+5, base+6,
		))
		args = append(args, u.WalletID, u.AvailableBalance, u.LockedBalance, u.KRW, u.Quantity, u.AvgBuyPrice)
	}

	sql := fmt.Sprintf(`
		UPDATE wallets AS w
		SET
			available_balance = v.available_balance,
			locked_balance = v.locked_balance,
			krw = v.krw,
			quantity = v.quantity,
			avg_buy_price = v.avg_buy_price
		FROM (VALUES %s) AS v(id, available_balance, locked_balance, krw, quantity, avg_buy_price)
		WHERE w.id = v.id`,
		strings.Join(rows, ", "),
	)

	result := r.DB.Exec(sql, args...)
	if result.Error != nil {
		return result.Error
	}
	if int(result.RowsAffected) != len(updates) {
		return fmt.Errorf("wallet batch update affected %d rows, expected %d", result.RowsAffected, len(updates))
	}
	return nil
}
```

(`strings` import 추가 필요.)

### 2. `internal/service/settlement_service.go` 수정

현재(4번의 개별 호출, 라인 178~189):

```go
		if err := walletRepo.UpdateBalances(participants.BuyerUserID, model.KRWAssetSymbol, buyerKRWUpdate.AvailableBalance, buyerKRWUpdate.LockedBalance); err != nil {
			return err
		}
		if err := walletRepo.UpdateBalancesAndAvgBuyPrice(participants.BuyerUserID, trade.CoinSymbol, buyerCoinUpdate.AvailableBalance, buyerCoinUpdate.LockedBalance, buyerCoinUpdate.AvgBuyPrice); err != nil {
			return err
		}
		if err := walletRepo.UpdateBalancesAndAvgBuyPrice(participants.SellerUserID, trade.CoinSymbol, sellerCoinUpdate.AvailableBalance, sellerCoinUpdate.LockedBalance, sellerCoinUpdate.AvgBuyPrice); err != nil {
			return err
		}
		if err := walletRepo.UpdateBalances(participants.SellerUserID, model.KRWAssetSymbol, sellerKRWUpdate.AvailableBalance, sellerKRWUpdate.LockedBalance); err != nil {
			return err
		}
```

다음과 같이 배치 호출 하나로 교체한다:

```go
		batchUpdates := []repository.WalletBatchUpdate{
			{WalletID: buyerKRW.ID, AvailableBalance: buyerKRWUpdate.AvailableBalance, LockedBalance: buyerKRWUpdate.LockedBalance, KRW: buyerKRWUpdate.KRW, Quantity: buyerKRWUpdate.Quantity, AvgBuyPrice: buyerKRWUpdate.AvgBuyPrice},
			{WalletID: buyerCoin.ID, AvailableBalance: buyerCoinUpdate.AvailableBalance, LockedBalance: buyerCoinUpdate.LockedBalance, KRW: buyerCoinUpdate.KRW, Quantity: buyerCoinUpdate.Quantity, AvgBuyPrice: buyerCoinUpdate.AvgBuyPrice},
			{WalletID: sellerCoin.ID, AvailableBalance: sellerCoinUpdate.AvailableBalance, LockedBalance: sellerCoinUpdate.LockedBalance, KRW: sellerCoinUpdate.KRW, Quantity: sellerCoinUpdate.Quantity, AvgBuyPrice: sellerCoinUpdate.AvgBuyPrice},
			{WalletID: sellerKRW.ID, AvailableBalance: sellerKRWUpdate.AvailableBalance, LockedBalance: sellerKRWUpdate.LockedBalance, KRW: sellerKRWUpdate.KRW, Quantity: sellerKRWUpdate.Quantity, AvgBuyPrice: sellerKRWUpdate.AvgBuyPrice},
		}
		if err := walletRepo.BatchUpdateBalances(batchUpdates); err != nil {
			return err
		}
```

(`buyerKRW`, `buyerCoin`, `sellerCoin`, `sellerKRW`는 이미 앞서 `FOR UPDATE`로 조회해 둔 `*model.Wallet`이라 `.ID` 필드를 그대로 쓸 수 있다.)

### 검증 절차 (사용자와 직접 진행, 이 스펙의 범위 밖)

1. TDD로 `BatchUpdateBalances`에 대한 리포지토리 레벨 테스트를 먼저 작성 — 여러 지갑을 한 번에 갱신하고 최종 값이 개별 갱신과 동일한지 확인.
2. `settlement_service.go`를 수정한 뒤, 기존 정산 통합 테스트(`TestIntegrationSettleTrade...` 7개)가 전부 그대로 통과하는지 확인 — 이게 회귀 검증의 핵심이다.
3. 배포 후 14/15번과 동일 조건으로 k6+`pg_stat_statements` 재측정 — 지갑 UPDATE 호출 횟수가 실제로 1/4로 줄었는지, 전체 CPU/처리량이 개선됐는지 확인.
4. 결과를 `docs/benchmarks/16-YYYY-MM-DD-wallet-batch-update.md`에 기록.

## 성공 기준

- 기존 정산 통합 테스트 7개가 전부 그대로 통과한다(동작 변화 없음).
- `pg_stat_statements`에서 지갑 UPDATE 호출 횟수가 실측으로 줄어든 게 확인된다.
- Postgres CPU 사용량이나 처리량에 측정 가능한 개선이 있는지 정직하게 기록한다(없어도 있는 그대로).

## 범위 밖 (Out of Scope)

- 주문 조회(2→1), 지갑 조회(4→1), 주문 갱신(2→1) 묶기 — 이번 결과를 보고 별도로 진행할지 결정.
- 매칭엔진 샤딩, Redis/Kafka 도입 — 이번 스코프가 아니다.
