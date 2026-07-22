# ⑤ 가용성 스파이크 벤치마크 측정 계획 (2차 리팩토링 · 23번 실증)

> **For agentic workers:** 이건 코드 TDD가 아니라 **GCP 측정 runbook**이다. hands-on 세션에서
> Phase 0→3을 순서대로, 체크박스(`- [ ]`)로 진행 상황을 갱신하며 실행한다. k6·정합성 검사는
> VM에서 독립 완주하므로 로컬 도구 지연에 영향받지 않는다(Phase 0 참조).

**Goal:** 22번 스파이크의 취소 43.9%·접수 p95 14.8s를 ①②③④가 고쳤음을 e2-highcpu-4에서
same-session A/B로 실증한다. 코드 변경 없음 — 측정과 문서화만.

**스펙 문서:** `docs/superpowers/specs/2026-07-22-availability-spike-benchmark-design.md`

## 확정 값

- **A(옛)** = `c899284` — 22번 배포 커밋. 코드가 2차 이전 main tip `0ff6aea`(=`ea7e8e6^`)과
  **완전 동일**(docs 제외 diff 0 확인 완료). A arm이 22번 baseline 바이너리를 그대로 재현한다.
- **B(신)** = 현 main HEAD(코드 = ④ tip `daf9d3e`; 이후 docs 커밋은 바이너리 무영향).
- **환경**: 서버 `e2-highcpu-4`, DB `e2-highcpu-4`, load-gen `e2-standard-8`, k6, 서울.
- **부하 프로파일**: `300 웜(1분) → 5,000 램프(30초) → 5,000 hold 3분 → ~10,000 버스트 45초
  → 300 급락(30초) → 회복 2분`.

## Global Constraints

- `DEV_TOOLS_TOKEN`·VM 외부 IP를 **트랜스크립트에 절대 노출 금지**. 커맨드엔 `$TOKEN`/`$SRV`
  같은 변수 플레이스홀더를 쓰고 값은 세션 로컬에만 둔다.
- 세션 내내 VM stop/start 없음(측정 A→B 연속). **측정 종료 후 VM stop**.
- 정합성 0은 비협상 — 위반 시 예단 없이 그대로 기록.
- 각 arm은 리셋(9테이블 TRUNCATE + backend 재시작 + `bootstrap loaded=0`) 후 측정.
- 커밋은 Phase 3에서 태스크 단위 commit-message 스킬(author→reviewer, 한글).

---

### Phase 0: 사전 점검 + k6 스크립트 준비

- [ ] **Step 1: 도구 디스패치 지연 회피 검증**(22번 교훈 #4) — gcloud ssh 스냅샷을
  harness `run_in_background`로 디스패치하면 9~18분 지연 재현. 착수 전 확인:
  간단한 `gcloud compute ssh $SRV --command 'date'`를 (a) 일반 호출과 (b) 로컬
  `& disown` 백그라운딩으로 각각 실행해 지연 유무 관측. hold/버스트 중반 스냅샷은
  전부 `& disown` 경로로 잡는다. (k6·정합성 검사는 VM에서 독립 완주하므로 무관.)
- [ ] **Step 2: VM 상태·A/B 커밋 확인** — 서버·DB·load-gen VM이 e2-highcpu-4/standard-8로
  떠 있는지, `git log -1 c899284`·현 main HEAD 확인. VM이 stop 상태면 start(같은 머신 타입).
- [ ] **Step 3: k6 스파이크 스크립트 작성 + 리포 커밋** — `_workspace/loadtest/order-spike-availability.js`
  (측정 후 벤치마크 문서에 첨부; 재현성 위해 리포에 남긴다). 22번 `order-spike-single-symbol.js`
  계승 + 5,000 목표·초과 버스트·503/Retry-After 계측. 스테이지·계측 요건:

  ```js
  // stages (ramping-vus)
  // { target: 300,   duration: '1m'  },   // 웜
  // { target: 5000,  duration: '30s' },   // 램프
  // { target: 5000,  duration: '3m'  },   // 목표 hold
  // { target: 10000, duration: '45s' },   // 초과 버스트
  // { target: 300,   duration: '30s' },   // 급락
  // { target: 300,   duration: '2m'  },   // 회복
  //
  // custom Counter: custom_order_success, custom_market_success,
  //   custom_cancel_success, custom_cancel_already_filled, custom_cancel_fail,
  //   custom_fast_reject_503, custom_reject_missing_retry_after
  //
  // POST /orders 응답 분류:
  //   200/201 → order_success (시장가면 market_success)
  //   503     → fast_reject_503; res.headers['Retry-After'] 없으면 reject_missing_retry_after++
  //   500     → (취소 경로면) cancel_fail
  //   404/409 → cancel_already_filled
  // thresholds: 차단 없음(런을 완주시켜 전부 기록). BASE_URL·DEV_TOOLS_TOKEN은 -e로 주입.
  ```

  스크립트를 load-gen VM에 `scp`. **검증**: `k6 run --vus 10 --duration 10s` 스모크로
  파싱·인증·커스텀 메트릭 집계가 도는지 확인(리셋 후 데이터는 버림).

---

### Phase 1: A arm (옛 코드, c899284)

- [ ] **Step 1: 배포 A** — `git archive c899284` → `scp` → `sudo tar` 추출 → 이전 bench `.env`
  복사 → `sudo docker compose --project-directory <bench-dir> -f docker-compose.stress.yml
  up -d --build backend`. 기동 로그로 커밋/구성 확인.
- [ ] **Step 2: 리셋** — 9테이블 `TRUNCATE ... RESTART IDENTITY CASCADE` + backend 재시작 +
  기동 로그 `bootstrap loaded=0` 확인.
- [ ] **Step 3: A 런** — `k6 run -e BASE_URL=http://$SRV:8080 -e DEV_TOOLS_TOKEN=$TOKEN
  ~/order-spike-availability.js`. 런 중 `& disown` 경로로: 채널 게이지(exec/order) 15초 샘플,
  CPU(서버+DB) hold 중반·버스트 중반, backend 로그 스트림 저장(스테이지별 지연 재구성용).
- [ ] **Step 4: 드레인 대기 + 정합성 5검사** — execution 채널·정산 워커 큐 0 확인 후:
  1. `/metrics`의 `reconciliation_violations{check="ledger_wallet"}` = 0 (원장-지갑)
  2. `reconciliation_violations{check="asset_conservation"}` = 0 (자산 총량 BTC/KRW)
  3. SQL `SELECT count(*) FROM failed_settlements` = 0
  4. SQL `SELECT count(*) FROM failed_market_completions` = 0
  5a. `reconciliation_violations{check="stale_market_order"}` = 0 **및** SQL 시장가
     PENDING/PARTIAL 잔존 = 0
  5b. SQL outbox PENDING 잔존 = 0 (outbox 테이블명·상태 컬럼은 실행 시 스키마 확인)
  5c. `/metrics`의 `settlement_batch_fallbacks_total` 확인
  (리컨실리에이션 워커는 주기 실행이므로 드레인 후 1주기 지난 게이지를 읽는다.)
- [ ] **Step 5: 수집** — k6 요약(custom 포함)·게이지 샘플·CPU·`RestartCount`
  (`docker inspect`)·backend 로그 아카이브를 `_workspace/`에 A 라벨로 저장. 스테이지별
  `POST /orders` 지연은 로그 `--since/--until` 윈도우로 재구성.

---

### Phase 2: B arm (신 코드, 현 main)

- [ ] **Step 1: 리셋 + 배포 B** — 리셋(9테이블 TRUNCATE + 재시작) → `git archive`(현 main HEAD)
  → 배포(Phase 1 Step 1과 동일 절차) → 리셋 재확인 `bootstrap loaded=0`.
- [ ] **Step 2: B 런** — Phase 1 Step 3과 **동일 스크립트·동일 샘플링**. 추가로 런 종료 직전
  `/metrics`에서 `orders_admission_rejected_total{stage="engine_gate"|"engine_handoff"|"coordinator"}`
  를 읽어 셰딩 분포 기록(리셋 전에).
- [ ] **Step 3: 드레인 + 정합성 5검사** — Phase 1 Step 4와 동일.
- [ ] **Step 4: 수집** — Phase 1 Step 5와 동일 + 셰딩 분포. pprof 30초는 best-effort(`& disown`).
- [ ] **Step 5: VM stop** — 서버·DB·load-gen 전부 stop(측정 종료).

---

### Phase 3: 분석 + 문서 + README + 메모리

- [ ] **Step 1: A/B 판정표 작성** — 스펙의 성공 기준 7항목을 A/B 실측으로 채운다:
  취소 실패율, 접수 p95(5,000 hold), 초과 버스트 응답(503+Retry-After·바운디드 여부),
  셰딩 분포(B), 정합성 5검사(A·B 각각), RestartCount, 회복 시간. **수치는 측정값 그대로**
  (접수 p95 바는 자릿수 개선이 본질; 5,000 셰딩은 0이든 일부든 그대로 — 천장 위치 보고).
- [ ] **Step 2: 벤치마크 문서** — `docs/benchmarks/23-<측정일>-availability-spike-ab.md`
  ([추적 관례](../../benchmarks/) 계승): 왜(22번 두 실패)·방식(same-session A/B, e2-highcpu-4,
  프로파일)·판정표·원본 출력(k6 A/B 요약, 셰딩 분포, 정합성, 스테이지별 지연)·해석·예상 밖
  발견(있으면 그대로). k6 스크립트(`_workspace/loadtest/…`) 경로 병기.
- [ ] **Step 3: 완료 문서 + README** — `docs/refactor/15_2차⑤_가용성_스파이크_실증_완료.md`
  (왜/어떻게/결과 — ①~④의 미뤄둔 수치가 채워졌음을 명기) + README 2차 표 ⑤ 🔲→✅ +
  "2차 리팩토링(가용성 100%) 완결" 표기.
- [ ] **Step 4: 메모리 정정** — `goexchange-performance-goal` 메모리의 VM 클래스
  ("고정 e2-medium/e2-small")가 실제 벤치마크(e2-highcpu-4)와 충돌 → 사용자와 프로덕션
  목표 하드웨어를 확인한 뒤 메모리를 사실과 일치시킨다.
- [ ] **Step 5: Commit + 푸시 + CI** — 문서 커밋(author→reviewer), `gh run watch` 그린.

---

## 판정 실패 시 (분기)

- **취소 실패율 > 0(B)**: ③ 우선순위가 스파이크에서 안 먹힌 것 — 로그로 원인(여전히
  CancelCh 타임아웃인지, 다른 경로인지) 규명 후 기록. 2차 후속으로 승격 검토.
- **정합성 위반(A 또는 B)**: 비협상 — 즉시 그대로 기록하고 근본 원인 조사(측정 중단 가능).
- **5,000에서 대량 셰딩(B)**: 천장이 5,000 아래 — 실제 천장을 보고. 응답이 바운디드면
  가용성 정의상 합격이나, 목표 미달이면 샤딩 코디네이터(백로그) 승격 근거가 된다.

## 범위 밖

- 히스테리시스(flapping 관측 시 조건부)·샤딩 코디네이터(셰딩 과다 시)·production
  e2-medium/e2-small 재측정·다중 심볼 스파이크.
