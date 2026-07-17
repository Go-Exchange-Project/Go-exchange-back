# OutboxWriter 처리율 개선 구현 계획

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** OutboxWriter의 배치 상한을 64 → 512(env 조정 가능)로 올려, 21번 벤치마크가 실측한 write-ahead 관문 캡(≈827 events/s, flush 상한 포화)을 푼다. 라이더로 기본 엔진 샤드 수를 1로 내린다(21번 p95 +57% 방어).

**Architecture:** 로직 무변경 — 바뀌는 것은 배치 상한 상수의 출처(`GOEXCHANGE_OUTBOX_BATCH_SIZE` env, 기본 512), 메트릭 버킷(128 → 1024까지), 그리고 `EngineShardsFromEnv`의 기본값(NumCPU → 1)뿐이다. 수집 루프·재시도·백프레셔·shutdown·리플레이 전부 무수정.

**스펙 문서:** `docs/superpowers/specs/2026-07-17-outbox-writer-throughput-design.md`

## Global Constraints

- OutboxWriter의 수집·재시도·forward·shutdown 로직 수정 금지 — 상한 값의 출처만 바꾼다. 기존 outbox writer·리플레이·크래시 계약 테스트가 무수정으로 통과해야 한다(로직 무변경의 증거).
- FlushInterval(5ms) 불변 — 저부하 지연 특성을 바꾸지 않는다.
- 정산 큐 크기(256) 불변 — 21번 교훈(깊은 버퍼 = 꼬리 지연). forward 블록은 의도된 백프레셔다.
- 병렬 writer는 이 계획 범위 밖(스펙의 "검토한 대안" — 22번 측정 결과가 조건).
- 통합 테스트: 테스트 DB + DSN(포트 55432), `-v`로 SKIP 0 확인.
- 커밋은 태스크 단위, CLAUDE.md의 `commit-msg-author` → `commit-msg-reviewer` 절차.

---

### Task 1: `GOEXCHANGE_OUTBOX_BATCH_SIZE` env + 배선 + 메트릭 버킷

**Files:**
- Modify: `config/runtime.go`, `config/runtime_test.go`
- Modify: `internal/metrics/metrics.go` (`trade_outbox_flush_batch_size` 버킷: `{1,2,4,8,16,32,64,128,256,512,1024}`)
- Modify: `cmd/main.go` (OutboxWriter 생성 시 `BatchSize: config.OutboxBatchSizeFromEnv()`)
- Modify: `internal/service/outbox_writer.go` (`defaultOutboxBatchSize` 64 → 512 — env 미설정·비정상 시의 최종 폴백)

- [x] **Step 1: 실패하는 테스트** — 3케이스 추가 → FAIL(undefined: OutboxBatchSizeFromEnv) 확인.
- [x] **Step 2: 구현** — env 상수+함수, `defaultOutboxBatchSize = 512`(21번 근거 주석), 메트릭 버킷 1024까지 확장, main.go `BatchSize: config.OutboxBatchSizeFromEnv()` 배선.
- [x] **Step 3: 통과 확인** — config·metrics PASS.
- [x] **Step 4: writer 대배치 동작 확인** — 기존 테스트에 대배치 케이스가 없어 `TestOutboxWriterFillsLargeBatchToCap`(600건 → 512+88 두 배치, 순서 보존) 추가. 기존 writer 테스트 6건 무수정 그린 포함 전부 PASS.
- [x] **Step 5: Commit** — Task 2와 같은 파일(config/runtime.go)을 건드려 커밋을 묶음: `perf(outbox): 그룹커밋 배치 상한을 512로 상향해 write-ahead 관문 풀기` (`1cf1206`, 라이더 포함. reviewer REDO 1회 — 테스트 수 서술 오류 정정 후 PASS)

---

### Task 2: 라이더 — 기본 엔진 샤드 수 1

**Files:**
- Modify: `config/runtime.go` (`EngineShardsFromEnv` 기본값 `runtime.NumCPU()` → `1`)
- Modify: `config/runtime_test.go` (기본값 테스트 갱신 — 이 테스트만 수정 허용)

- [x] **Step 1**: 기본값 테스트를 1 기대로 갱신(`TestEngineShardsFromEnvDefaultsToSingleShard`) → 기본값 1 + 21번 근거 주석 → PASS. 미사용이 된 `runtime` import 정리(runtime.go/runtime_test.go).
- [x] **Step 2**: config + matching PASS (ShardedEngine 테스트 무영향 확인).
- [x] **Step 3: Commit** — Task 1과 묶어 `1cf1206`에 포함(같은 파일 변경이라 분리 시 add -p 필요 — 본문에 명시적 서술로 대체).

---

### Task 3: 전체 검증 + 문서 + 푸시

- [x] **Step 1**: build + vet 클린, 전체 스위트 12개 패키지 ok(통합 포함, SKIP 0), service·cmd `-race` PASS.
- [x] **Step 2**: 완료 문서 9번 작성 + README 현황판 9번 행 추가, 백로그 2건 취소선(병렬 writer는 22번 조건부 잔여로 명기).
- [x] **Step 3**: Commit + 푸시 + CI 그린.

---

## 성능 측정 (범위 밖 — 22번 사이클)

같은 바이너리 env 스위프 A/B: `GOEXCHANGE_OUTBOX_BATCH_SIZE=64`(A) vs `512`(B), 필요 시 128/256/1024 — 재빌드 없는 A/B라 배포 변수까지 제거된 비교. 판정: ① flush 평균이 새 상한 대비 여유(포화 해제) ② execution 채널 만석 해제 ③ 처리량 상승 폭 ④ 다음 병목 식별(DB CPU 258% 예상). 여전히 flush 포화 + 채널 만석이면 병렬 writer(2단계) 승격.
