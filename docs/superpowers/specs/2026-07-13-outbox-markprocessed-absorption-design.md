# MarkProcessed 흡수: outbox 마킹을 정산 트랜잭션에 합치기

- 작성: 2026-07-13 (Fable5 설계·구현)
- 로드맵: [docs/refactor/README.md](../../refactor/README.md) 백로그
- 근거: [17번 벤치마크](../../benchmarks/17-2026-07-13-trade-outbox-throughput-impact.md)

## 왜 필요한가

A-3 outbox 도입으로 처리량이 ~17% 떨어졌다(17번 벤치마크, 램프업/hold 두 프로파일
일관). 원인 규명 결과, outbox INSERT는 group commit으로 배치화돼(44.7건/배치, 왕복
991회) 저렴했고, **진짜 비용은 정산 후 trade당 개별로 나가는 `MarkProcessed`
UPDATE 44,300회**였다. 이 환경은 DB/CPU가 병목이라 추가 왕복이 곧 처리량 저하다.

## 핵심 아이디어

지금: `SettleTrade`(트랜잭션 A 커밋) → 워커가 별도 `MarkProcessed`(트랜잭션 B).
바꿈: outbox 마킹을 트랜잭션 A 안으로 흡수 → trade당 DB 왕복 2회 → 1회.

정산과 마킹이 원자적이 되므로 부수 효과로 견고해진다: "정산 성공했는데 마킹 실패로
재처리"되는 창이 사라진다(재처리는 멱등이라 위험하진 않았지만, 아예 없어짐).

## 적용 범위: 성공 경로만

정산 트랜잭션이 롤백되면 그 안의 마킹도 롤백된다. 따라서 흡수는 **정산 성공(applied)
또는 멱등 중복(duplicate)** 경로에서만 가능하다. 정산 실패 후 내구기록(failed_settlements)
경로는 정산 트랜잭션이 이미 롤백된 상태라, 기존처럼 워커가 별도 MarkProcessed를
호출한다. 벤치마크에서 44,300건 전부 성공(실패율 0%)이었으므로 성공 경로 흡수만으로
−17%의 실질 전부를 회수한다.

## 변경 지점

1. **`SettleTrade(trade, outboxEventID uint64)`**: 시그니처에 outboxEventID 추가.
   트랜잭션 콜백의 3개 성공 return 지점(applied 1 + duplicate 2)을
   `return markSettledOutbox(tx, outboxEventID)`로 교체. 헬퍼는 outboxEventID==0이면
   no-op, >0이면 같은 tx로 `UPDATE trade_outbox_events SET status='PROCESSED',
   processed_at=now() WHERE id=?`. 라이브 경로의 outboxEventID는 OutboxWriter가 방금
   INSERT·커밋한 행 ID라 항상 존재한다(RowsAffected==0 방어는 불필요 — 발생 불가).

2. **`processExecutionEvent(event, outboxEventID, ...) (handled, markedInTx bool)`**:
   - trade 성공 → `SettleTrade(trade, outboxEventID)`가 tx 안에서 마킹 → markedInTx=true
   - trade 실패+내구기록 → tx 롤백돼 마킹 안 됨 → markedInTx=false (워커가 fallback)
   - 시장가 완료 → CompleteMarketOrder(outbox 안 넘김) → markedInTx=false (fallback)

3. **라이브 워커**(cmd/main.go): `handled, markedInTx := processExecutionEvent(...,
   outboxEvent.OutboxID, ...)`. `markedInTx`면 별도 MarkProcessed 스킵.

4. **리플레이어**: Process 콜백은 outboxEventID=0으로 호출 → 마킹 안 함 →
   리플레이어가 기존대로 MarkProcessed. 부팅 경로라 성능 무관, 동작 불변.

5. **재시도 워커**: `retryTradeSettler.SettleTrade(trade, uint64)` 시그니처 맞추고
   `SettleTrade(trade, 0)` 호출(outbox와 무관).

## 스코프 밖

- **시장가 완료(CompleteMarketOrder) 흡수**: 벤치마크(지정가 전용)에 없었고 소량이며,
  order_service 트랜잭션까지 변경하면 범위가 커진다. 기존 fallback 마킹 유지.
  필요해지면(시장가 비중이 큰 부하가 문제되면) 후속으로.
- outbox 보존 정리(PROCESSED 아카이빙) — 별도 백로그.

## 검증 방법

1. **단위**: markSettledOutbox가 outboxEventID=0이면 no-op. SettleTrade with
   outboxID>0의 성공/중복/실패 경로에서 markedInTx 반환값.
2. **통합(실제 Postgres)**: ① outbox PENDING 행 + SettleTrade(trade, outboxID) →
   trade 정산 + outbox PROCESSED가 **한 트랜잭션에** 커밋됐는지(정산 성공 & outbox
   PROCESSED). ② 정산 실패(예: 잔고 부족) → 트랜잭션 롤백으로 outbox가 PENDING
   유지되는지(흡수가 원자적임을 증명). ③ 기존 정산 통합 테스트 회귀.
3. **전체 스위트** `go test ./... -count=1`.
4. **GCP 재측정**(선택): 17번과 같은 hold 프로파일로 A-3(37a3e49) vs 이 최적화 →
   처리량이 pre-A-3(162.7 iter/s) 수준으로 회복되는지. VM stop 상태라 start 필요.
