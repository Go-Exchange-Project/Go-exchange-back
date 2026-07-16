# 체결 브로드캐스트 배치화 (B-1c) 구현 계획

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 체결 WS 브로드캐스트를 정산 배치(B-4) 단위로 묶는다. 와이어 형식을 `{"type":"trade","data":{...}}` × N에서 `{"type":"trades","data":[{...},...]}` × 1(심볼당)로 전환해, 고부하에서 클라이언트별 채널 송신·프레임 write를 최대 32:1로 줄인다. 19번 벤치마크가 측정한 WS fanout 병목(−33.5%)의 해소가 목적이다.

**Architecture:** 변경 지점은 두 곳뿐이다 — ① 프론트 `Index.tsx`에 `trades` 배열 핸들러 추가(선배포, 양형식 수용), ② 백엔드 `cmd/main.go`의 `broadcastSettledTrade`를 심볼 그룹핑 배치 헬퍼 `broadcastSettledTrades`로 교체. **hub/ws 패키지는 무변경** — 심볼 라우팅(B-1b)·클라이언트 관리 그대로.

**스펙 문서:** `docs/superpowers/specs/2026-07-16-trade-broadcast-batching-design.md`

## Global Constraints

- **배포 순서가 호환성이다**: 반드시 프론트(양형식 수용)를 먼저 커밋·배포하고 백엔드를 전환한다. 역순이면 프론트 갱신 전까지 체결 내역 피드가 멈춘다.
- `"type":"trade"` 단건 형식은 완전히 은퇴시킨다 — 폴백·리플레이·재시도 등 단건 경로도 원소 1개짜리 `trades` 형식을 쓴다. 형식은 하나만 유지한다.
- 배열 원소의 필드는 기존 `trade` 메시지의 `data` 객체와 **정확히 동일**(coin_symbol, engine_sequence, engine_event_id, idempotency_key, price, quantity, fee_rate, buyer_fee, buyer_fee_asset, seller_fee, seller_fee_asset, time). 프론트 `toTradeHistoryEntry`가 원소 단위로 그대로 재사용돼야 한다.
- 한 정산 배치에 여러 심볼이 섞일 수 있다(워커 큐는 심볼 해시 파티션) — 심볼별로 그룹핑하되 **배치 내 순서를 보존**해 심볼당 1메시지로 발행한다.
- 새 타이머·고루틴·버퍼 금지 — 묶음의 원천은 B-4 배치뿐이다.
- GCP 측정은 이 계획 범위 밖(다음 사이클에 다중 심볼 프로파일 작업과 일괄).
- 커밋은 CLAUDE.md의 `commit-msg-author` → `commit-msg-reviewer` 절차(두 리포 모두, 메시지는 한글).

---

### Task 1: 프론트 — `trades` 배열 핸들러 추가 (Go-exchange-front, 선배포)

**Files:**
- Modify: `src/pages/Index.tsx`

- [x] **Step 1**: WS 메시지 union 타입(Index.tsx:59 부근)에 `{ type: "trades"; data?: TradeMessageData[] }` 추가.
- [x] **Step 2**: 핸들러 체인(`data.type === "trade"` 분기 옆)에 `"trades"` 분기 추가 — 배열을 **순서대로 순회**하며 기존 단건 처리 로직(`toTradeHistoryEntry` + 심볼 필터 + 삽입)을 재사용한다. 기존 `"trade"` 분기는 **유지**(백엔드 전환 전까지의 양형식 수용).
- [x] **Step 3**: 빌드/린트 통과 확인 (`npm run build` 또는 리포의 기존 검증 커맨드).
- [x] **Step 4**: Commit (author→reviewer 절차) — 초안: `feat(ws): 체결 배치 메시지(trades) 수신 지원 추가` (`49b9f74`, 푸시 완료)

---

### Task 2: 백엔드 — `broadcastSettledTrades` 배치 헬퍼로 교체

**Files:**
- Modify: `cmd/main.go`
- Modify: `cmd/main_test.go`

- [x] **Step 1: 실패하는 단위 테스트 작성** (`main_test.go`, 기존 broadcast 페이크 패턴 재사용):

```go
// 혼합 심볼 배치 → 심볼당 1메시지, 배치 내 순서 보존, coinSymbol 태그 일치
func TestBroadcastSettledTradesGroupsBySymbolPreservingOrder(t *testing.T)
// 페이로드 형식: type=trades, data는 배열, 원소 필드는 기존 trade 메시지의 data와 동일
func TestBroadcastSettledTradesPayloadFormat(t *testing.T)
// 빈 슬라이스 → 발행 0건
func TestBroadcastSettledTradesEmptyIsNoop(t *testing.T)
```

- [x] **Step 2: 실패 확인** — Run: `go test ./cmd/... -run TestBroadcastSettledTrades -v` → FAIL (undefined: broadcastSettledTrades, 3곳)
- [x] **Step 3: 구현** — `broadcastSettledTrade`를 `broadcastSettledTrades(trades []*model.Trade, broadcast func(string, []byte), logger *log.Logger)`로 교체:
  - 심볼별 그룹핑(순서 보존: 등장 순서대로 심볼 키 수집), 심볼당 `{"type":"trades","data":[...]}` 마샬 1회 → `broadcast(symbol, json)`.
  - 마샬 실패는 기존과 동일하게 로그만 남기고 건너뜀(정산 내구성과 무관).
  - 호출부 교체: `settleTradeBatchWithFallback`는 Applied trade들을 슬라이스로 모아 **1회 호출**(main.go:546-550의 루프 제거), `processTradeSettlement`(단건 경로)는 원소 1개 슬라이스로 호출. 옛 헬퍼 삭제.
- [x] **Step 4: 통과 확인** — Run: `go test ./cmd/... -run TestBroadcastSettledTrades -v` → PASS (3개 전부)
- [x] **Step 5: 회귀** — `go build ./...` OK. `go test ./cmd/... ./internal/ws/... -count=1` → PASS (ws 패키지 무변경·무영향의 증거). 통합 포함 전체 `go test ./... -count=1 -v`(테스트 DB `docker compose -f docker-compose.test.yml up -d --wait` 기동, `GOEXCHANGE_TEST_DATABASE_DSN` 설정) → **390 PASS / 0 SKIP / 0 FAIL** (SKIP 0건으로 통합 테스트가 실제 DB에서 돌았음을 확인).
- [x] **Step 6: 로컬 E2E 수동 확인** — 로컬 백엔드(임시 postgres, 포트 5433) + 프론트 dev 서버(localhost:3000) 기동 후, 헤드리스 브라우저가 없어 `ws://localhost:8080/ws`에 직접 연결하는 Node 스크립트로 원본 메시지를 캡처: 유저 2명 생성·충전 후 지정가 매도/매수 쌍 2회 제출 → 실서버가 `"type":"trades","data":[...]`만 발행(레거시 `"type":"trade"` 0건)함을 확인. 필드 구성(`coin_symbol`/`engine_sequence`/`fee_rate`/... )도 기존과 동일. 확인 후 임시 DB·dev 서버 정리.
- [ ] **Step 7: Commit** (author→reviewer 절차) — 초안: `perf(ws): 체결 브로드캐스트를 정산 배치 단위로 묶어 fanout 비용 축소 (B-1c)`

---

### Task 3: 문서 + 푸시

- [ ] **Step 1**: `docs/refactor/7_B-1c_체결_브로드캐스트_배치화_완료.md` 작성(왜 문제였나: 19번 해석 2 / 어떻게: 스펙 링크 + 요지 / 결과: 테스트 요약, GCP 측정은 다음 사이클 병기). README 현황판에 B-1c 행 추가(✅, 19번에서 발견된 병목의 후속임을 명기), 기존 7번(B-3)은 8번으로 밀림.
- [ ] **Step 2**: Commit (author→reviewer 절차) + 두 리포 푸시 + `gh run watch`로 백엔드 CI 그린 확인.

---

## 성능 측정 (범위 밖)

다음 GCP 사이클에서 다중 심볼 부하 프로파일 작업(숙제 ②)과 묶어 측정: 측정 4 조건(legacy×300) 재현 A/B로 WS 부하 시 하락폭(−33.5%)이 줄어드는지, `ws_messages_received`가 ~32:1로 감소하는지 확인.
