# B-3: 매칭 엔진 심볼 샤딩 설계

- **날짜**: 2026-07-17
- **상태**: 설계 검토 중
- **근거**: [20번 벤치마크](../../benchmarks/20-2026-07-16-multi-symbol-remeasurement.md) 엔진 병목 판정 — 매칭 CPU는 0.52%로 미미하지만 `order` 채널이 hold 8분 내내 버퍼 상한(1024)에 고정. **단일 goroutine이 8개 심볼을 직렬 처리하는 구조 자체가 처리량 캡**(1,479 iter/s에서 포화).

## 목표와 불변식

심볼을 N개의 독립 엔진 goroutine에 분배해 직렬화 캡을 푼다. 단, 매칭의 본질인
**"한 심볼은 정확히 하나의 goroutine이 소유한다"** 는 절대 불변이다 — 이것이
오더북 무락 접근, 심볼 내 가격-시간 우선순위, trade→Done 순서 보존의 근거다.

유지해야 할 불변식:
1. **심볼 내 순서**: 같은 심볼의 주문 처리·이벤트 방출 순서는 지금과 동일(심볼당 단일 goroutine + 채널 FIFO).
2. **write-ahead(A-3)**: 정산은 outbox에 커밋된 이벤트만. 백프레셔(ExecutionCh가 차면 매칭 블록) 유지.
3. **graceful shutdown 도미노**: HTTP 닫힘 → 엔진 드레인 → ExecutionCh/SnapshotCh close → OutboxWriter flush → 큐 close → 워커 드레인.
4. **멱등성 키 전역 고유**: `EngineEventID = "<engineID>-<seq>"`인데 `engineID`가 인스턴스별 고유(nano 타임스탬프+atomic 카운터, engine.go:549-552)이므로 **샤드 N개가 각자 생성해도 충돌 없음 — 코드 수정 불필요** (이번 설계의 핵심 사전 검증 항목이었음). `EngineSequence`는 샤드별 단조지만 심볼이 샤드에 고정되므로 심볼 내 단조성은 유지된다.

## 설계: 라우터 + 기존 엔진 N개 (엔진 내부 무수정)

**`MatchingEngine`은 한 줄도 고치지 않는다.** 새 타입 `ShardedEngine`(internal/matching/sharded.go)이 기존 엔진 N개를 소유하고 심볼을 라우팅한다:

```go
type ShardedEngine struct {
    shards      []*MatchingEngine
    assignments sync.Map // coinSymbol -> *MatchingEngine
    nextShard   atomic.Uint64
    ExecutionCh chan ExecutionEvent    // 팬인(머지) 채널, cap 1024
    SnapshotCh  chan OrderBookSnapshot // 팬인(머지) 채널, cap 256
    ...
}
```

### 1. 심볼 → 샤드 배정: first-seen 라운드로빈

fnv 해시(정산 큐 방식)가 아니라 **처음 보는 심볼을 라운드로빈으로 배정**하고
`sync.Map`에 고정한다(`LoadOrStore`). 이유: 20번에서 해시 파티션의 불균형(10개
워커 중 2개에만 큐 적체)을 실측했고, B-3의 효과 측정이 8심볼×N샤드에서
이뤄지므로 해시 충돌로 샤드 2개에 심볼이 몰리면 측정 자체가 왜곡된다.
배정은 프로세스 생애 동안 불변(제출·취소·조회가 같은 샤드로) — 재시작 시
배정이 바뀔 수 있지만 오더북은 부트스트랩이 재구축하므로 무관하다.

### 2. 이벤트 팬인: 샤드별 채널 → 머지 채널

각 샤드는 기존대로 자기 `ExecutionCh`/`SnapshotCh`를 갖고 stop 시 자기 것을
닫는다(기존 코드 그대로). `ShardedEngine.Start()`가 샤드당 포워더 goroutine
(`for ev := range shard.ExecutionCh { merged <- ev }`)을 띄우고, 전원 종료 시
(`WaitGroup`) 머지 채널을 닫는다.

- **심볼 내 순서 보존**: 한 심볼의 이벤트는 한 샤드에서만 나오고, 그 샤드의
  포워더 goroutine 하나가 순서대로 옮긴다(Go 채널은 송신자별 순서 보존).
- **백프레셔 보존**: OutboxWriter가 밀리면 머지 채널이 차고 → 포워더 블록 →
  샤드 ExecutionCh가 차고 → 해당 샤드 매칭 블록. A-3 의도 그대로.
- **다운스트림 무수정**: OutboxWriter는 `Source`가 머지 채널로 바뀔 뿐이고,
  main의 SnapshotCh 소비 루프도 동일. 심볼 간 전역 순서는 인터리브되지만
  애초에 보장 대상이 아니었다(정산 큐가 심볼 파티션인 이유).

### 3. 제출·취소·조회 라우팅

`OrderService`가 현재 `me.OrderCh <- order`로 직접 채널 전송하므로, 라우팅을
위해 메서드 경유로 바꾼다:

```go
// matching 패키지에 최소 인터페이스 — 구현 2개(MatchingEngine, ShardedEngine)
type Engine interface {
    SubmitOrder(*Order)
    CancelOrder(CancelOrderCommand) CancelOrderResult
    RequestOrderBookSnapshot(coinSymbol string, depth int) (OrderBookSnapshot, error)
}
```

- `MatchingEngine.SubmitOrder`는 `me.OrderCh <- order` 한 줄(기존 동작 그대로).
  기존 단일 엔진을 쓰는 테스트들은 인터페이스를 그대로 만족한다.
- `ShardedEngine`은 세 메서드 모두 배정 맵으로 샤드를 찾아 위임한다.
  스냅샷 조회도 해당 샤드의 캐시(sync.Map)에서 읽으므로 여전히 락-프리.
- 빈 심볼(테스트·레거시 경로)은 샤드 0으로 고정.

### 4. 종료 도미노 (스코프만 확장)

`ShardedEngine.Stop()` → 모든 샤드 `Stop()`(각자 드레인 후 자기 채널 close) →
포워더들이 잔여 이벤트를 머지 채널로 옮기고 종료 → `WaitGroup` 완료 시 머지
채널 close → OutboxWriter가 잔여 배치 flush 후 반환(기존 도미노 그대로).
`Done()`은 머지 채널이 닫힌 뒤 닫힌다. main의 30초 상한 로직 무변경.

### 5. 샤드 수

`GOEXCHANGE_ENGINE_SHARDS` 환경변수, 기본 `runtime.NumCPU()`
(`SettlementWorkersFromEnv` 패턴). 매칭은 CPU가 아니라 직렬화가 병목이므로
(0.52%) 코어 수보다 많아도 무해하다 — 벤치마크에서는 8(심볼당 1개)로 측정한다.

### 6. 메트릭

기존 4개 채널 게이지는 **샤드 합산**으로 유지(대시보드 호환), 추가로
`matching_engine_shard_order_channel_length{shard="i"}` GaugeVec 1개 — 20번에서
채널 게이지가 병목을 잡았으므로 샤드별 적체 가시성은 필수다(불균형 탐지 겸용).

## 검토한 대안

- **fnv 해시 배정(정산 큐 방식)**: 상태 없는 3줄이지만 8심볼 소규모에서 충돌
  불균형이 실측돼 있고(20번), 측정 왜곡 리스크가 라운드로빈 맵의 복잡도(~15줄)
  보다 비싸다. 기각.
- **엔진 내부에 샤딩 내장**(OrderBooks 맵을 goroutine별 분할): 검증된 엔진
  코드를 대수술해야 하고, 라우터 방식과 결과가 같다. 기각.
- **공유 ExecutionCh(팬인 없이 N샤드가 한 채널에 직접 송신)**: 포워더가
  없어지지만 stop 시 close 소유권 문제(각 샤드가 공유 채널을 닫으면 double
  close panic)로 엔진 내부 수정이 필요해진다. 엔진 무수정 원칙과 교환 불가. 기각.
- **정산 워커 불균형 동시 수정**: 20번의 부수 관찰이지만 B-3와 독립된 변경이고
  정산은 현재 병목 증거가 약하다(10개 중 2개 적체는 처리량 캡으로 이어진 증거
  없음). 백로그로 — B-3 재측정에서 워커 큐가 병목으로 드러나면 같은 라운드로빈
  방식을 적용한다.

## 검증 계획

1. 단위: 배정 안정성(같은 심볼=항상 같은 샤드, 라운드로빈 균등), 제출·취소·조회가 같은 샤드로 위임, 팬인의 심볼 내 순서 보존, 빈 심볼 → 샤드 0.
2. 종료: Stop() 후 머지 ExecutionCh/SnapshotCh가 잔여 이벤트 전달을 완료하고 닫히는지(기존 Stop 배리어 테스트 패턴), 드레인 유실 0.
3. 동시성: 다심볼 동시 제출 `-race`(기존 engine_concurrency_test 패턴을 ShardedEngine으로).
4. 기존 스위트 전부 그린 — 특히 부트스트랩·outbox 통합·크래시 계약 테스트(단일 엔진 테스트는 무수정으로 통과해야 함 = 엔진 무수정의 증거).
5. **GCP 재측정(다음 사이클)**: A=현 main, B=B-3(`GOEXCHANGE_ENGINE_SHARDS=8`), 20번과 같은 다중 심볼 hold 프로파일 same-session A/B. 판정 신호: ① 샤드별 order 채널이 상한(1024) 고정에서 풀리는지 ② 처리량이 1,479 iter/s 캡을 넘는지. 예고: 엔진 캡이 풀리면 다음 병목은 주문 생성 경로의 DB 왕복(pprof 50%)일 가능성이 높다 — B-3의 이득이 그 지점에서 멈추는 것은 실패가 아니라 다음 항목의 근거다.
