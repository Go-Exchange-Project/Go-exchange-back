# A-3: trade outbox + graceful shutdown 설계

- 작성: 2026-07-12 (Fable5 설계·구현)
- 로드맵: [docs/refactor/README.md](../../refactor/README.md) 4번

## 왜 필요한가

체결(trade)과 시장가 완료(MarketOrderDone) 이벤트가 엔진 → 정산 사이에서
**인메모리 채널에만** 존재한다. 프로세스가 죽으면(크래시, OOM, 배포 재시작)
채널과 정산 큐에 있던 이벤트가 전부 증발한다:

- trade 유실 → 매수자 코인 미지급 + 매도자 KRW 미지급 (체결 기록 자체가 없어짐)
- MarketOrderDone 유실 → 시장가 잔여 hold 영구 동결 (A-1이 만든 내구 기록은
  "완료 시도가 실패했을 때"만 남는다 — 시도 전에 죽으면 기록이 없다)

배포할 때마다 이 유실 창이 열린다. 남은 마지막 정합성 구멍이다.

## 핵심 불변식

**정산은 outbox에 커밋된 이벤트만 처리한다 (write-ahead).**

이 불변식이 서면 크래시 타이밍이 어디든 자금 정합성이 유지된다:

- **outbox 커밋 이전 크래시**: 이벤트 유실 = 그 매칭은 "없던 일"이 된다.
  정산이 안 됐으므로 지갑·원장·주문의 filled_amount 모두 무변동 —
  재부팅 시 부트스트랩이 DB의 미체결 주문을 재투입해 다시 매칭될 수 있다.
  돈은 한 푼도 움직이지 않았으므로 정합성 위반이 아니다.
- **outbox 커밋 이후 크래시**: 리플레이가 PENDING 이벤트를 재처리한다.
  정산 멱등성 키(engine event ID)가 있으므로 이미 정산된 이벤트의 재처리는
  no-op — 최소 1회 전달(at-least-once) + 멱등 = 정확히 1회 효과.

기존 아키텍처 결정(리뷰 시점)과의 정합: Kafka 단일 브로커는 Postgres COMMIT
대비 내구성 이득이 0이면서 dual-write 문제를 만든다. Postgres 테이블 outbox가
정석 관문이고, 추후 CDC(Debezium 등)로 외부 소비자를 붙일 경로도 열어 둔다.

## 아키텍처

```
[기존]  engine ─ExecutionCh→ dispatcher ─→ queues[N] ─→ settlement workers
[변경]  engine ─ExecutionCh→ OutboxWriter ─→ dispatcher ─→ queues[N] ─→ workers
                              (배치 INSERT,      (OutboxEvent{ID, event})     └→ MarkProcessed
                               커밋 후 전달)
```

### 1. TradeOutboxEvent 모델

```go
type TradeOutboxEvent struct {
    ID          uint64    // BIGSERIAL — 단일 writer라 삽입 순서 = 엔진 방출 순서
    EventType   string    // TRADE | MARKET_ORDER_DONE
    CoinSymbol  string    // 심볼 파티셔닝 라우팅용
    EngineEventID string  // trade만, 추적용(빈 문자열 허용)
    Payload     []byte    // jsonb — Trade 또는 MarketOrderDone 직렬화
    Status      string    // PENDING | PROCESSED
    CreatedAt   time.Time
    ProcessedAt *time.Time
}
```

- status+id 부분 인덱스(`WHERE status='PENDING'`)로 리플레이 조회 최적화.
- PROCESSED 행은 남긴다(감사 추적). 보존 정리는 추후 별도(스코프 외).

### 2. OutboxWriter (단일 goroutine)

- `ExecutionCh`를 소비해 배치로 모은다: **64건 또는 5ms** 중 먼저 도달하는
  시점에 한 트랜잭션으로 INSERT (group commit — B-4의 선행 형태).
- 커밋 성공 후에만 각 이벤트를 `OutboxEvent{ID, ExecutionEvent}`로 감싸
  기존 심볼 파티셔닝 디스패처에 전달한다.
- INSERT 실패 시 무한 재시도(50ms→1s 지수 백오프). 이 동안 ExecutionCh(1024)가
  차면 엔진 매칭이 블록된다 — **의도된 백프레셔**다. DB가 죽었는데 매칭만
  계속되면 유실 대기 이벤트가 메모리에 무한 적체된다. 돈이 우선이다.
- 크래시 창 분석: writer가 배치를 INSERT하기 전에 죽으면 → 불변식의
  "커밋 이전" 케이스(매칭 롤백, 정합성 유지). INSERT 후 전달 전에 죽으면 →
  PENDING으로 남아 리플레이가 처리.

### 3. 부팅 리플레이 + 시장가 파이널라이저 (순서 중요)

부팅 시 라이브 파이프라인 개시 **전에**:

1. **리플레이**: PENDING 행을 ID 순으로 **순차** 처리(기존
   processExecutionEvent 재사용) 후 PROCESSED 마킹. 순차라서 심볼별
   FIFO(trade들→Done)가 자명하게 보존된다. PENDING 잔량은 크래시 시점의
   in-flight뿐이라(큐 상한 ~수천) 순차로 충분하다.
2. **시장가 파이널라이저**: 리플레이가 끝난 시점에 DB에 PENDING/PARTIAL로
   남은 MARKET 주문은 엔진 메모리가 사라졌으므로 **더 이상 체결될 수 없다**.
   각각 CompleteMarketOrder를 호출해 잔여 hold를 해제한다(완료량은 DB에서
   자체 계산되고 멱등이므로 안전). 이것으로 "trade는 outbox에 남았지만 Done은
   못 남기고 크래시"한 시장가의 잔여 hold 영구 동결이 해소된다 —
   리컨실리에이션 검사 3(stale market order)이 잡던 마지막 케이스의 자동 복구.
3. 그 다음 부트스트랩(LIMIT 재투입) → HTTP 개시.

파이널라이저가 리플레이 **뒤**여야 하는 이유: 리플레이 중인 trade 정산이
끝나기 전에 완료를 시도하면 filled 검증 conflict가 난다(순서가 곧 정확성).

### 4. Graceful shutdown

`signal.NotifyContext(SIGINT, SIGTERM)` 후 종료 체인:

```
HTTP Shutdown(10s) → engine.Stop() [OrderCh/CancelCh 드레인 후 루프 종료,
close(ExecutionCh), close(SnapshotCh)] → OutboxWriter [range 종료 → 잔여 배치
flush → 각 queue close] → workers [range 종료, WaitGroup 대기] → 백그라운드
워커들 context cancel → 종료. 전체 상한 30s(초과 시 로그 남기고 강제 종료 —
outbox 덕에 유실은 없고 다음 부팅 리플레이가 처리).
```

- 엔진 루프가 ExecutionCh·SnapshotCh의 유일한 writer이므로 루프 종료 후
  close가 안전하다. close가 downstream 종료를 도미노로 전파한다.
- gin `r.Run()` → `http.Server` + `ListenAndServe`로 전환(Shutdown 지원).

### 5. 메트릭

- `trade_outbox_flush_seconds` (histogram): 배치 INSERT 소요.
- `trade_outbox_flush_batch_size` (histogram): 배치 크기 — group commit 효율 관측.
- `trade_outbox_write_errors_total` (counter): INSERT 실패(재시도 전 단위).
- 기존 `matching_engine_execution_ch_length`·워커 큐 게이지로 적체 관측.

## 스코프 외 (하지 않는 것)

- 주문 접수 자체의 로그화(주문은 이미 DB 선저장 후 엔진 투입 — 내구적).
- PROCESSED 행 보존 기간 관리/아카이빙.
- CDC/Kafka 연동(경로만 열어 둠), 정산 그룹커밋(B-4 — outbox와 별개로 정산
  트랜잭션 자체의 배칭), WS 브로드캐스트 경로 변경(B-1).

## 검증 방법

1. **단위**: writer 배치 경계(64건/5ms), 커밋 후에만 전달, INSERT 실패 재시도,
   리플레이 순서·마킹, 파이널라이저 대상 선정.
2. **통합(실제 Postgres)**: ① PENDING 행 주입 → 리플레이 → 정산 완료 +
   PROCESSED + 리컨실리에이션 위반 0건. ② 같은 리플레이 2회 실행 → 멱등(중복
   정산 없음). ③ 체결 일부만 outbox에 남은 시장가 → 리플레이+파이널라이저 →
   잔여 hold 해제 + 상태 확정.
3. **크래시 주입(프로세스 수준)**: 서버 기동 → 주문 체결 직후 SIGKILL →
   재기동 → 리플레이 로그 + 지갑/원장 정합(리컨실리에이션 0건) 확인.
4. **graceful**: SIGTERM 중 in-flight 주문/체결 유실 0, 30s 내 종료.
5. **k6 전후 비교**: outbox 쓰기 추가로 인한 정산 처리량/레이턴시 영향 측정
   (`docs/benchmarks/` 컨벤션) — GCP 배포 후 별도 수행.

## 검증 결과 (2026-07-12 구현 완료 후 실측)

- **단위 테스트**: writer 배치 경계(크기/간격/close 시 잔여 flush)·커밋 후에만
  전달·INSERT 무한 재시도·직렬화 왕복 — 리플레이어 순서·keyset 페이지네이션·
  내구 미확정 PENDING 유지·손상 행 격리 — 파이널라이저 잔여 예산 계산·실패의
  내구 기록 위임 — 엔진 Stop 드레인 후 채널 close·멱등 Stop — 전부 통과.
- **통합 테스트** (실제 Postgres): outbox CRUD(ID 순서 = 삽입 순서, PENDING 필터,
  keyset 커서), **PENDING trade 리플레이 → 실제 정산 완결 → 중복 리플레이 시
  이중 정산 0** (exactly-once), 파이널라이저의 시장가 잔여 hold 해제 + 원장
  ORDER_RELEASE 기록 — 전부 통과. 전체 스위트 `go test ./... -count=1` 그린.
- **결정론적 크래시 주입** (실제 서버 바이너리, "outbox 커밋 직후·정산 직전"
  상태를 SQL로 재현 후 부팅): `replayed=2` 로그와 함께 체결이 정확히 1회 정산
  (매수자 BTC +1, 매도자 KRW 99,950 — 수수료 50원까지 정확), 주문 FILLED,
  outbox PROCESSED. Done을 못 남긴 시장가는 `finalized=2`로 CANCELLED 확정 +
  locked 50,000 → available 50,000 해제. **A-1이 남겼던 마지막 영구 동결
  케이스가 자동 복구로 닫힘.**
- **무작위 SIGKILL 3회** (체결 버스트 40/60/150건 도중 taskkill /F): 매회
  재기동 후 해당 유저 원장-지갑 gap 0행, PENDING outbox 0, 미해소 주문 0 —
  불변식("커밋 전 크래시 = 매칭 롤백, 커밋 후 = 리플레이 완결")이 유지됨.
- **graceful shutdown** (리눅스 컨테이너 PID1, 체결 버스트 도중 SIGTERM):
  `shutdown: signal received, draining pipeline` → `shutdown complete`,
  **exit code 0, 체결 30/30 전부 정산, PENDING outbox 0, 미해소 주문 0** —
  배포 재시작 시 유실 창이 닫혔고, 강제 종료돼도 다음 부팅 리플레이가 처리한다.
