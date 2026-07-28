# 3차 최종 검증: 가용성 스파이크 재측정 runbook (23번 재실행)

> **For agentic workers:** 측정 runbook이다. Phase 0→5를 순서대로, 체크박스(`- [ ]`)로 진행한다.
> **애플리케이션 코드 무수정**(k6 스크립트는 측정 도구라 인덱스 분할 파라미터 추가만 허용).

**Goal:** 3차 리팩토링(③ SLI 3분할 · ① 취소 진행성 · ②-수정 정산 병렬화 + 신규 지갑 데드락 수정)의
효과를 **사용자 관점 SLI 3종 + 정합성**으로 최종 판정한다. 특히 **3차에서 유일하게 미실증인
"①이 취소 인프라 실패를 실제로 줄였는가"**(23번 A/B에서 B=48.67%)를 `sli_cancel_success`로 답한다.

**선행 문서:** `docs/benchmarks/23-2026-07-23-availability-spike-ab.md`(비교 기준),
`docs/refactor/16·17·18·19_*.md`(무엇이 바뀌었나), `docs/benchmarks/25-*.md`(정산 병렬화 실측).

## Global Constraints

- **애플리케이션 코드 무수정.** 서버 env·k6 스크립트(측정 도구)만.
- 서버·DB는 **23번과 동일**(e2-highcpu-4, 서울) — 23번 수치와 비교 가능해야 한다. 서버 구성은
  **바꾸지 않는다**(바꾸면 비교가 깨진다).
- `GOEXCHANGE_SETTLEMENT_CONCURRENCY`는 **권장값 4**(25번 판정). 기동 로그로 실제 적용값 확인.
- **정합성 5검사 + `settlement_batch_fallbacks_total` 0**이 비협상. 위반 시 그것이 최우선 결과.
- 수치는 그대로 기록. 개선이 없거나 역행해도 미화 금지.
- `DEV_TOOLS_TOKEN`·외부 IP 트랜스크립트 노출 금지. 측정 후 VM stop.

---

### Phase 0: load-gen 수평 증설 (핵심 변경)

**왜 수평인가 — 수직은 이미 실패했다.** 23번에서 load-gen이 ~9,800 VU(버스트 램프 98%)에서 3회
죽었고, **커널 OOM·panic 흔적이 없어** 원인을 **네트워크 커넥션 생성 속도(per-VM 한계)** 로 추정했다.
그때 이미 `e2-standard-8 → e2-standard-16`으로 **수직 증설했는데도 재발**했다. 따라서 이번엔
**부하 생성을 2대로 쪼개** per-VM 커넥션 생성률을 절반으로 낮춘다.

- [ ] **Step 1: load-gen 2대 준비** — `e2-standard-8` **2대**(A/B). 서버·DB VM은 23번과 동일하게 유지.
  각 VM에 k6 설치·스크립트 배포(같은 커밋의 같은 파일).
- [ ] **Step 2: 유저 인덱스 분할(스크립트 파라미터 추가)** — 현재 스크립트는
  `spike-user-${i}`(i=1..`TOTAL_USERS`)를 만들고 VU를 `exec.vu.idInTest`로 매핑한다. 두 VM이 그대로
  돌면 **같은 유저를 공유**해 스크립트가 피하려던 "1 VU = 1 user"(지갑 잔고 경합 혼선 방지)가 깨진다.
  → `USER_INDEX_OFFSET`(기본 0) env를 추가해 유저 인덱스를 `OFFSET+1 .. OFFSET+TOTAL_USERS`로 만들고,
  VM A는 `OFFSET=0`, VM B는 `OFFSET=5000`으로 띄운다. **각 VM의 `TOTAL_USERS`는 그 VM이 낼 피크 VU와 같게.**
- [ ] **Step 3: VU 분할** — 23번 프로파일의 각 스테이지 target을 **VM당 절반**으로 설정
  (`300→150`, `5000→2500`, `10000→5000`, …). 합계가 23번과 같아야 비교가 성립한다.
  버스트 피크에서 **VM당 ~5,000** — 23번이 죽은 ~9,800의 절반이라 관측된 절벽 아래다.
- [ ] **Step 4: 시나리오 시작 시각 정렬 (`LOAD_START_AT_MS` 배리어)** —
  **`k6 run` 명령 시각을 맞추는 것으로는 부족하다.** k6는 각 프로세스에서
  `프로세스 시작 → setup()(5,000명 등록·로그인·펀딩) → 시나리오 시작` 순으로 돌기 때문에,
  **setup 소요 시간 차이가 그대로 ramp 시작 skew가 된다**(5,000명 setup은 수 분대라 VM·네트워크
  편차가 쉽게 수 초를 넘는다). 따라서 **setup 이후에 공통 시각까지 대기하는 배리어**를 넣는다:

```js
const LOAD_START_AT_MS = parseInt(__ENV.LOAD_START_AT_MS || '0', 10);

// setup() 마지막 — 유저 생성·펀딩을 끝낸 뒤 공통 시작 시각까지 대기한다.
const remainingMs = LOAD_START_AT_MS - Date.now();
if (LOAD_START_AT_MS > 0 && remainingMs <= 0) {
  throw new Error('setup missed the coordinated load start deadline'); // 이 런은 폐기
}
if (remainingMs > 0) {
  sleep(remainingMs / 1000);
}
```

  - 두 VM에 **같은 `LOAD_START_AT_MS`**(UTC epoch ms)를 주고, 값은 **양쪽 setup 예상 시간보다 충분히
    뒤로** 잡는다. setup이 그 시각을 넘기면 **즉시 폐기 후 재실행**(위 throw가 강제).
  - 두 VM 시계 동기(`timedatectl`) 먼저 확인.
  - **실제 시나리오 시작 시각은 양쪽 k6 로그에서 다시 확인**하고 기록한다(배리어가 작동했다는 증거).
  - **skew 기준: 목표 ≤1초, 허용 상한 2초, 2초 초과면 폐기 후 재실행**(30초 ramp 대비 비율을 감안).
- [ ] **Step 5: 서버 preflight** — `GOEXCHANGE_ENABLE_PPROF=true`, 기동 로그에서
  `settlement partitions=.. concurrency=4` 확인, `/metrics` 노출 확인, 리셋(9테이블 TRUNCATE +
  `bootstrap loaded=0`).

---

### Phase 1: 측정 실행

- [ ] **Step 1: 부하** — 두 VM에서 `order-spike-availability.js` 실행(각자 절반 VU, 분할된
  `USER_INDEX_OFFSET`, **같은 `LOAD_START_AT_MS`**). setup은 VM별로 자기 범위만 처리한다.
  **구조화된 요약을 반드시 저장**한다(콘솔 텍스트를 사람이 옮겨 적어 합산하지 않는다):

```bash
k6 run --summary-export summary-a.json -e USER_INDEX_OFFSET=0    -e LOAD_START_AT_MS=<epoch> ...
k6 run --summary-export summary-b.json -e USER_INDEX_OFFSET=5000 -e LOAD_START_AT_MS=<epoch> ...
```
- [ ] **Step 2: 서버측 샘플(15초 간격)** — `matching_engine_channel_length{execution|order}` ·
  `settlement_worker_queue_length` · `orders_admission_rejected_total{stage}` ·
  `settlement_batch_fallbacks_total` · 서버/DB CPU · DB 커넥션 사용량.
- [ ] **Step 3: 스테이지별 지연 재구성** — 23번과 동일하게 **서버 도커 로그 `--since/--until`** 로
  `POST /orders`의 스테이지별 p50/p95/max와 상태 분포(200/503/500)를 재구성. **이 신호는 서버측이라
  load-gen 분할의 영향을 받지 않는다** — 판정의 주 근거로 삼는다.
- [ ] **Step 4: 취소 결과** — 서버 로그에서 `DELETE /orders/:id`의 200/409/404/500 분포.
  **500(인프라 실패)의 로그 시그니처**(`duration=1s` = CancelCh 타임아웃인지) 확인 — ①의 효과를
  가르는 결정적 증거.
- [ ] **Step 5: 드레인 + 정합성 5검사** — 채널·큐 0 확인 후 리컨실리에이션 2종 ·
  `failed_settlements` · `failed_market_completions` · 시장가 잔존 · outbox 잔존 ·
  **`settlement_batch_fallbacks_total`(19번 수정 후 0이어야 함)**.
- [ ] **Step 6: load-gen 생존 확인** — 두 VM이 프로파일을 **완주**했는지(23번은 여기서 죽었다).
  죽었다면 Phase 3의 "천장 보고" 분기로.

---

### Phase 2: SLI 집계 (두 VM 합산)

세 SLI는 **비율**이라 단순 평균이 아니라 **분자·분모를 합산**해야 한다. `--summary-export`로 저장한
두 JSON에서 각 Rate의 `passes`/`fails`를 읽어 합산 재계산한다(콘솔 텍스트 수기 전사 금지):

```
combined rate = (A.passes + B.passes) / (A.passes + A.fails + B.passes + B.fails)
```

- [ ] **Step 1** — `sli_order_response_availability` = (A통과+B통과) / (A전체+B전체)
- [ ] **Step 2** — `sli_order_business_success` = (A 2xx + B 2xx) / (A전체+B전체)
- [ ] **Step 3** — `sli_cancel_success` = (A 200 + B 200) / (A 200+실패 + B 200+실패)
  (404/409는 양쪽 모두 분모 제외 — 스크립트가 이미 처리)
- [ ] **Step 4: 교차 검증** — 합산 SLI가 **서버 로그 기반 상태 분포**(Phase 1 Step 3·4)와 방향이
  일치하는지 확인. 어긋나면 클라이언트 집계가 아니라 **서버측 수치를 신뢰**하고 그 사실을 기록.
- [ ] **Step 5: 원본 보존** — 양쪽 **stdout·`summary-*.json`·실제 시나리오 시작/종료 시각**(배리어
  작동 증거)을 `_workspace/` 측정 원본에 함께 보관한다. **`DEV_TOOLS_TOKEN`·외부 IP는 제거**한 뒤 저장.

---

### Phase 3: 판정

- [ ] **Step 1: 취소 성공률(3차의 핵심 미실증)** — `sli_cancel_success`를 **23번 B(48.67% 실패)**
  와 비교. ①(하류 인지 게이트)이 취소 굶주림을 줄였는가? 500의 시그니처가 여전히 `duration=1s`면
  P2가 남은 것이고, 사라졌으면 ①이 효과를 낸 것이다. **①은 "완화이지 일반 보장 아님"으로 스펙에
  적었으므로, 0이 아니어도 실패가 아니라 "얼마나 줄었나"가 판정 대상.**
- [ ] **Step 2: 응답 가용성** — `sli_order_response_availability`(≤1s 계약). 23번 B는 성공·거절
  모두 ms 단위였다 — **유지되는가**(①의 게이트가 하류 조건까지 보게 됐으니 셰딩이 더 일찍 걸릴 수 있음).
- [ ] **Step 3: 업무 성공률** — `sli_order_business_success`. 23번 B는 hold에서 **6.9%**(셰딩 93.1%).
  **정산 병렬화(N=4)가 이 숫자를 올렸는가**가 ②의 사용자 관점 효과다.
- [ ] **Step 4: 정합성** — 5검사 + fallbacks 전부 0. 하나라도 위반이면 **최우선 결과**로 문서 최상단.
- [ ] **Step 5: 천장 보고(분기)** — load-gen이 또 죽었으면 **수평 증설로도 못 넘은 부하 수준**을
  그대로 기록하고, 판정은 **완주한 구간까지의 데이터**로 한다(23번의 "크래시 직전 데이터로 판정" 선례).
  10,000을 억지로 채우려 재시도를 반복하지 말 것.

---

### Phase 4: 문서 + README

- [ ] **Step 1: 벤치마크 문서** — `docs/benchmarks/26-<측정일>-availability-spike-remeasurement.md`:
  왜(3차 최종 검증) / 방법(**load-gen 수평 2대 분할·인덱스 분할·시각 정렬**, 서버 구성은 23번 동일) /
  SLI 3종 합산 결과 + **23번 B와의 대비표** / 스테이지별 지연·상태 분포 / 정합성 / 판정 4종 /
  한계(분할 집계·시작 skew·완주 여부).
- [ ] **Step 2: 완료 문서 보강** — 17번(①)의 "취소 0 실증은 재측정" 문장에 **이 결과 링크와 한 줄**을
  추가해 미뤄둔 수치를 닫는다. 16번(③)에도 SLI 3종 첫 실전 판정 링크 추가.
- [ ] **Step 3: README** — 3차 표에 최종 검증 결과 반영. 남은 병목·미해결(P2 잔존 여부 등)은 백로그로.
- [ ] **Step 4: Commit + 푸시 + CI** — Conventional Commits·한글(스킬 불가 시 직접 작성).
- [ ] **Step 5: 정리** — **load-gen 2대 포함 모든 VM stop**(증설분을 켜둔 채 잊지 말 것).

---

## 다음 (범위 밖)

Phase 3에서 P2가 잔존으로 판정되면 **일반적 취소 진행 보장**(주문당 emit 상한 또는 재개형 매칭 —
①의 스펙이 후속으로 남긴 항목) 설계. N=8 다른 락 경로 재검증(19번이 범위 밖으로 남김).
관측성: 라이브 배치 정산 경로를 관측 못 하는 `order_settlement_duration_seconds_count`(24번 발견).
