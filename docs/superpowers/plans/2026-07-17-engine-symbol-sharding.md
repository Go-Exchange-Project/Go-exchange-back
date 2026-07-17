# 매칭 엔진 심볼 샤딩 (B-3) 구현 계획

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 심볼을 N개 독립 엔진 goroutine에 first-seen 라운드로빈으로 분배하는 `ShardedEngine`을 구현해, 20번 벤치마크가 판정한 단일 goroutine 직렬화 캡(order 채널 상시 1024 포화, 1,479 iter/s)을 푼다.

**Architecture:** 기존 `MatchingEngine`은 **한 줄도 수정하지 않는다**. 새 파일 `internal/matching/sharded.go`의 `ShardedEngine`이 엔진 N개를 소유하고 심볼을 라우팅하며, 샤드별 ExecutionCh/SnapshotCh를 팬인 포워더로 머지 채널에 합친다. 소비자(OrderService·OrderBookHandler·부트스트랩)는 새 `matching.Engine` 인터페이스를 받는다(구현 2개: 단일·샤딩). 다운스트림(OutboxWriter, 스냅샷 소비 루프, 정산 큐)은 무수정.

**스펙 문서:** `docs/superpowers/specs/2026-07-17-engine-symbol-sharding-design.md`

## Global Constraints

- `internal/matching/engine.go`의 기존 코드는 수정 금지 — 유일한 허용 추가는 `SubmitOrder(*Order)` 메서드(`me.OrderCh <- order` 한 줄)와 인터페이스 선언이다. 기존 엔진 단위·동시성 테스트가 무수정으로 통과해야 한다(엔진 무수정의 증거).
- 심볼 → 샤드 배정은 first-seen 라운드로빈 + `sync.Map` 고정. 같은 심볼의 제출·취소·스냅샷 조회는 반드시 같은 샤드로. 빈 심볼("")은 샤드 0 고정.
- 팬인 포워더는 샤드당 1 goroutine — 심볼 내 이벤트 순서가 보존되는 유일한 구조다. 여러 goroutine이 한 샤드 채널을 경쟁 소비하는 구조 금지.
- 머지 채널 close는 모든 포워더 종료 후 1회(WaitGroup) — 샤드가 자기 채널을 닫는 기존 코드는 그대로 두고, 머지 채널의 close 소유권은 ShardedEngine에만 있다.
- 백프레셔 보존: 머지 ExecutionCh는 블로킹 송신(cap 1024). SnapshotCh 포워딩도 기존 의미론(엔진 쪽은 이미 논블로킹 발행이므로 포워더는 그냥 블로킹 복사면 됨, cap 256).
- 샤드 수: `GOEXCHANGE_ENGINE_SHARDS`, 기본 `runtime.NumCPU()`, `config/runtime.go`의 `parsePositiveIntEnv` 패턴.
- 통합 테스트: 테스트 DB 컨테이너 + DSN(포트 55432) 설정, `-v`로 SKIP 0 확인.
- GCP 측정은 범위 밖(다음 사이클, 스펙의 검증 계획 5 참조).
- 커밋은 태스크 단위, CLAUDE.md의 `commit-msg-author` → `commit-msg-reviewer` 절차.

---

### Task 1: `matching.Engine` 인터페이스 + 소비자 전환 (동작 불변 리팩터링)

**Files:**
- Modify: `internal/matching/engine.go` (SubmitOrder 메서드 + 인터페이스 선언만 추가)
- Modify: `internal/service/order_service.go` (필드 `*matching.MatchingEngine` → `matching.Engine`, `OrderCh <-` → `SubmitOrder()`)
- Modify: `internal/service/matching_bootstrap_service.go` (제출 경로 확인 후 동일 전환)
- Modify: `internal/handler/order_book_handler.go` (필드 타입 → `matching.Engine`)
- Modify: `cmd/main.go` (컴파일 맞추기 — 아직 단일 엔진 그대로)

- [x] **Step 1**: `matching` 패키지에 추가:

```go
// Engine은 매칭 엔진의 소비자 표면이다. 구현: MatchingEngine(단일), ShardedEngine(B-3).
type Engine interface {
	SubmitOrder(*Order)
	CancelOrder(CancelOrderCommand) CancelOrderResult
	RequestOrderBookSnapshot(coinSymbol string, depth int) (OrderBookSnapshot, error)
}

// SubmitOrder는 주문을 엔진 루프에 넘긴다(기존 OrderCh 직접 송신과 동일 의미).
func (me *MatchingEngine) SubmitOrder(order *Order) { me.OrderCh <- order }
```

- [x] **Step 2**: 소비자 3곳의 필드·파라미터 타입을 `matching.Engine`으로 바꾸고 `OrderCh <-` 직접 송신을 `SubmitOrder()`로 교체. 부트스트랩의 제출 경로는 먼저 코드를 읽고 같은 방식으로. (`order_book_handler.go`는 이미 자체 `OrderBookSnapshotProvider` 최소 인터페이스를 쓰고 있어 변경 불필요 — `matching.Engine`을 만족하는 어떤 구현체도 그대로 호환됨. `cmd/main.go`도 무변경으로 컴파일됨. 부트스트랩의 `ctx.Done()` 취소 가능 제출은 `SubmitOrder`를 goroutine으로 감싸고 `submitted` 채널로 select해 동일 동작 보존.)
- [x] **Step 3**: 검증 — `go build ./...` 통과 + **기존 테스트 전부 무수정 통과**: `go test ./... -count=1 -v` → 전 패키지 PASS, SKIP 0(통합 테스트 실행 확인). `git status`로 테스트 파일 무변경 확인(동작 불변의 증거).
- [x] **Step 4**: Commit (author→reviewer 절차) — `refactor(matching): 매칭 엔진 소비자 표면을 Engine 인터페이스로 분리 (B-3 사전 작업)` (`955f5ee`, 푸시 완료)

---

### Task 2: `ShardedEngine` 코어

**Files:**
- Create: `internal/matching/sharded.go`
- Create: `internal/matching/sharded_test.go`

- [x] **Step 1: 실패하는 단위 테스트 작성** (기존 engine_test의 짧은 티커 패턴 `newTestEngine` 참고 — ShardedEngine 생성자에 스냅샷 간격 주입이 필요하면 내부 생성 헬퍼로):

```go
// 같은 심볼은 항상 같은 샤드, 서로 다른 첫 심볼 N개는 서로 다른 샤드(라운드로빈 균등)
func TestShardedEngineAssignsSymbolsRoundRobinAndStably(t *testing.T)
// 빈 심볼은 샤드 0
func TestShardedEngineRoutesEmptySymbolToShardZero(t *testing.T)
// 한 심볼에 주문 여러 건 → 머지 ExecutionCh에서 그 심볼의 trade들이 제출 순서대로 나온다
func TestShardedEnginePreservesPerSymbolEventOrder(t *testing.T)
// 서로 다른 심볼의 주문이 각자 체결되고 이벤트가 전부 머지 채널에 도착한다
func TestShardedEngineMatchesAcrossShardsIndependently(t *testing.T)
// 취소가 제출과 같은 샤드로 라우팅된다(제출→취소→오더북에서 사라짐)
func TestShardedEngineCancelRoutesToOwningShard(t *testing.T)
// 스냅샷 조회가 해당 샤드 캐시에서 읽힌다
func TestShardedEngineSnapshotReadsOwningShardCache(t *testing.T)
// Stop(): 접수된 주문을 전부 드레인한 뒤 머지 Execution/Snapshot 채널이 닫힌다(유실 0)
func TestShardedEngineStopDrainsAllShardsThenClosesMergedChannels(t *testing.T)
```

- [x] **Step 2: 실패 확인** — Run: `go test ./internal/matching/... -run TestShardedEngine -v` → FAIL (undefined: ShardedEngine, NewShardedEngine)
- [x] **Step 3: 구현** (`sharded.go`) — 스펙의 구조 그대로:
  - `NewShardedEngine(shardCount int)`: 내부에서 `NewMatchingEngine()` N개 생성. 머지 채널 cap: Execution 1024, Snapshot 256.
  - `shardFor(coinSymbol)`: `assignments.Load` 히트 시 그 샤드, 미스 시 `nextShard` atomic 증가로 후보를 정해 `LoadOrStore`(경쟁 시 먼저 저장된 쪽이 이긴다 — 배정 유일성은 LoadOrStore가 보장).
  - `Start()`: 샤드 전부 `Start()` + 샤드당 Execution/Snapshot 포워더 goroutine(각각 WaitGroup) → 두 그룹 완료 시 각 머지 채널 close, 둘 다 닫히면 `doneCh` close.
  - `SubmitOrder`/`CancelOrder`/`RequestOrderBookSnapshot`: `shardFor`로 위임.
  - `Stop()`: `sync.Once`로 전 샤드 `Stop()` 호출(도미노는 포워더 종료가 이어받음). `Done()`은 doneCh 반환.
  - `MatchLatencyObserver` 전파: 세터 또는 생성 시 주입으로 전 샤드에 동일 옵저버 설정.
  - 샤드별 채널 길이 접근자(Task 3의 메트릭용): `ShardOrderChannelLens() []func() int` 등.
- [x] **Step 4: 통과 확인** — Run: `go test ./internal/matching/... -run TestShardedEngine -v` → PASS(7개 전부). `TestShardedEngineCancelRoutesToOwningShard`는 최초 시도에서 캐시가 다음 코얼레싱 티커 전이라 실패 → 취소 후 스냅샷을 한 번 더 기다리도록 수정 후 PASS.
- [x] **Step 5: 동시성** — `TestShardedEngineConcurrentMultiSymbolSubmission_NoRace`(8심볼×20건 동시 제출, Stop/Done 배리어) 추가 후: `go test ./internal/matching/... -race -count=1` → PASS
- [x] **Step 6**: Commit (author→reviewer 절차) — `feat(matching): 심볼 샤딩 ShardedEngine 추가 (B-3)`

---

### Task 3: 설정 + main.go 전환 + 메트릭

**Files:**
- Modify: `config/runtime.go`, `config/runtime_test.go` (`EngineShardsFromEnv`, 기본 `runtime.NumCPU()`)
- Modify: `internal/metrics/metrics.go` (+ 테스트) — 샤드별 order 채널 GaugeVec `matching_engine_shard_order_channel_length{shard}`
- Modify: `cmd/main.go`

- [x] **Step 1**: `EnvGOExchangeEngineShards` + `EngineShardsFromEnv()` (TDD: 기본값=NumCPU/오버라이드/비정상 폴백 3케이스 — `ReconciliationIntervalFromEnv` 테스트 패턴).
- [x] **Step 2**: 메트릭 추가(TDD) — 기존 4개 채널 게이지는 유지하되 main에서 **샤드 합산 클로저**로 등록(대시보드 호환), 신규 GaugeVec는 샤드 인덱스 라벨. `RegisterMatchingEngineShardOrderChannelLenGauges`를 `testutil.GatherAndCompare`로 검증.
- [x] **Step 3**: `cmd/main.go` 전환 — `matching.NewShardedEngine(config.EngineShardsFromEnv())`, MatchLatencyObserver 주입, 게이지 등록(합산+샤드별), OutboxWriter `Source`·스냅샷 소비 루프·`Stop()/Done()`은 머지 채널/라우터 메서드로 그대로 연결. 기동 로그에 샤드 수 출력.
- [x] **Step 4: 전체 검증** — `go build ./...` + `go vet` 클린; `go test ./... -count=1 -v`(통합 포함) → 12개 패키지 전부 ok, SKIP 0, FAIL 0; `go test ./internal/matching/... ./internal/service/... ./cmd/... -race -count=1` PASS. 부트스트랩·outbox 통합 테스트 그린(도미노·write-ahead 불변의 증거).
- [x] **Step 5**: Commit (author→reviewer 절차) — `feat(matching): 서버 파이프라인을 ShardedEngine으로 전환 (B-3)`

---

### Task 4: 문서 + 푸시

- [x] **Step 1**: `docs/refactor/8_B-3_매칭_엔진_심볼_샤딩_완료.md` (왜: 20번 판정 / 어떻게: 스펙 요지 / 결과: 테스트 요약 + "성능 실증은 다음 측정 사이클" 병기 — 처리량 수치를 주장하지 않는다). README 8번 ✅.
- [x] **Step 2**: Commit (author→reviewer 절차) + 푸시 + CI 그린 확인. 커밋: `632455a`(코어), `f3d2818`(파이프라인 전환), 문서 커밋은 이 커밋.

---

## 성능 측정 (범위 밖 — 다음 사이클)

21번 벤치마크: A=현 main, B=B-3(`GOEXCHANGE_ENGINE_SHARDS=8`), 20번 다중 심볼 hold 프로파일 same-session A/B. 판정: ① 샤드별 order 채널 게이지가 1024 고정에서 풀리는지 ② 1,479 iter/s 캡 돌파 여부 ③ 다음 병목 식별(pprof — 주문 생성 DB 왕복 50%가 후보).
