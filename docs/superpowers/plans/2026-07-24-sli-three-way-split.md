# 3차 ③ SLI 3분할 구현 계획 (측정 규율)

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:executing-plans로 태스크별 실행.
> Steps use checkbox (`- [ ]`). 이건 k6(JS) 계측 변경 — Go 코드 무변경.

**Goal:** 뭉뚱그린 가용성을 세 k6 `Rate` SLI로 분리해 스파이크 스크립트에 baked-in 한다.
분류 로직은 순수 모듈로 빼 k6 셀프체크로 판정표를 덮는다. threshold는 안 건다(정의+기준선).

**스펙 문서:** `docs/superpowers/specs/2026-07-24-sli-three-way-split-design.md`

## Global Constraints

- **Go 코드·서버 메트릭 무변경** — k6 스크립트와 문서만. 기존 Counter·`http.expectedStatuses` 유지.
- Rate threshold 안 건다(합격 목표는 재측정 후). whole-run SLI(스테이지 태깅 없음).
- 지표명에 order/cancel 스코프 명시. "전체" = 전체 주문 제출 시도.
- 응답 가용성 시간은 `res.timings.duration`(Retry-After `sleep()` 제외 — res 수신 후 sleep).
- 커밋은 태스크 단위 commit-message 스킬(author→reviewer, 한글). Bash 실패 시 PowerShell.

---

### Task 1: 분류 순수 모듈 + k6 셀프체크

**Files:**
- Create: `_workspace/loadtest/sli-classify.js` (순수 함수, k6 import 없음)
- Create: `_workspace/loadtest/sli-classify.selftest.js` (k6로 실행하는 셀프체크)

**Interfaces:**
- `classifyOrderResponse(status, durationMs, sloMs) → { available: bool, businessSuccess: bool }`
- `classifyCancelResponse(status) → 'success' | 'excluded' | 'infra_fail'`

- [x] **Step 1: 셀프체크 먼저(판정표 각 행)** — `sli-classify.selftest.js`:

```js
import { classifyOrderResponse, classifyCancelResponse } from './sli-classify.js';

export const options = { vus: 1, iterations: 1 };

function assert(cond, msg) {
  if (!cond) throw new Error('SLI selftest FAILED: ' + msg);
}

export default function () {
  const SLO = 1000;
  // 주문 판정표
  let r = classifyOrderResponse(201, 500, SLO);
  assert(r.available && r.businessSuccess, '201 ≤SLO → 가용·업무 성공');
  r = classifyOrderResponse(201, 24000, SLO);
  assert(!r.available && r.businessSuccess, '201 >SLO → 가용 실패·업무 성공(느린 2xx)');
  r = classifyOrderResponse(200, 130, SLO);
  assert(r.available && r.businessSuccess, '200 ≤SLO → 둘 다 성공');
  r = classifyOrderResponse(503, 0.2, SLO);
  assert(r.available && !r.businessSuccess, '503 ≤SLO → 가용 성공·업무 실패');
  r = classifyOrderResponse(503, 1500, SLO);
  assert(!r.available && !r.businessSuccess, '503 >SLO → 둘 다 실패');
  r = classifyOrderResponse(0, 90000, SLO);
  assert(!r.available && !r.businessSuccess, 'status 0 → 둘 다 실패');
  // 취소 판정표
  assert(classifyCancelResponse(200) === 'success', '취소 200 → success');
  assert(classifyCancelResponse(404) === 'excluded', '취소 404 → excluded');
  assert(classifyCancelResponse(409) === 'excluded', '취소 409 → excluded');
  assert(classifyCancelResponse(0) === 'infra_fail', '취소 status 0 → infra_fail');
  assert(classifyCancelResponse(500) === 'infra_fail', '취소 500 → infra_fail');
  assert(classifyCancelResponse(502) === 'infra_fail', '취소 5xx → infra_fail');
  console.log('SLI selftest PASSED');
}
```

Run: `k6 run _workspace/loadtest/sli-classify.selftest.js` → FAIL(모듈 없음).

- [x] **Step 2: 순수 함수 구현** — `sli-classify.js`:

```js
// 응답 상태·지연을 두 SLI 판정으로 분류(순수 — k6 의존 없음, 셀프체크가 검증).
export function classifyOrderResponse(status, durationMs, sloMs) {
  const contracted = status === 200 || status === 201 || status === 503;
  return {
    available: contracted && durationMs <= sloMs, // 느린 2xx/503도 가용 실패
    businessSuccess: status === 200 || status === 201, // 503은 업무 실패(지연 무관)
  };
}

// 취소 성공률 분류: 404/409는 정상 경쟁이라 분모 제외, 그 외 비-200(status 0·5xx 포함)은 인프라 실패.
export function classifyCancelResponse(status) {
  if (status === 200) return 'success';
  if (status === 404 || status === 409) return 'excluded';
  return 'infra_fail';
}
```

Run: `k6 run _workspace/loadtest/sli-classify.selftest.js` → PASS(`SLI selftest PASSED`, exit 0).

- [x] **Step 3: Commit** — 초안: `test(loadtest): SLI 분류 순수 함수 + k6 셀프체크 추가 (3차 ③)`

---

### Task 2: 스파이크 스크립트에 3 Rate 통합

**Files:**
- Modify: `_workspace/loadtest/order-spike-availability.js`

- [x] **Step 1: import·상수·Rate 선언** — 상단에:

```js
import { Rate } from 'k6/metrics';
import { classifyOrderResponse, classifyCancelResponse } from './sli-classify.js';

const RESPONSE_SLO_MS = parseInt(__ENV.RESPONSE_SLO_MS || '1000', 10);

const orderResponseAvailability = new Rate('sli_order_response_availability');
const orderBusinessSuccess = new Rate('sli_order_business_success');
const cancelSuccessSli = new Rate('sli_cancel_success');
```

(기존 `Counter` import·카운터·`http.expectedStatuses`는 그대로 유지.)

- [x] **Step 2: 주문 SLI 배선** — `submitOrder`에서 `res` 수신 직후, **503 sleep 전에** 분류·add
  (모든 주문 흐름이 `submitOrder`를 거치므로 한 곳에서 계측):

```js
  const res = http.post(/* ... 기존 그대로 ... */);
  const cls = classifyOrderResponse(res.status, res.timings.duration, RESPONSE_SLO_MS);
  orderResponseAvailability.add(cls.available);
  orderBusinessSuccess.add(cls.businessSuccess);
  check(res, { /* 기존 그대로 */ });
  if (res.status === 503) {
    // ... 기존 fastReject503·Retry-After sleep 그대로 (sleep은 add 뒤라 지연에 안 섞임) ...
  }
```

- [x] **Step 3: 취소 SLI 배선** — `makerFlow`의 `cancelRes` 처리 분기에 add(404/409는 add 안 함):

```js
  const cancelClass = classifyCancelResponse(cancelRes.status);
  if (cancelClass === 'success') cancelSuccessSli.add(true);
  else if (cancelClass === 'infra_fail') cancelSuccessSli.add(false);
  // 'excluded'(404/409) → add 안 함 → 분모 자연 제외
  // 기존 cancelSuccess/cancelAlreadyFilled/cancelFail Counter는 그대로 유지
```

- [x] **Step 4: 로컬 스모크** — 로컬 backend 기동 후 소규모 오버라이드로 짧게:
  `k6 run -e BASE_URL=... -e DEV_TOOLS_TOKEN=... -e STAGE1_VUS=5 -e STAGE1_DURATION=5s
  -e STAGE2_VUS=5 -e STAGE3_VUS=5 -e STAGE3_DURATION=5s -e STAGE4_VUS=5 -e STAGE4_DURATION=5s
  -e STAGE5_VUS=5 -e STAGE6_VUS=5 -e STAGE6_DURATION=5s -e TOTAL_USERS=20 order-spike-availability.js`
  → 요약에 `sli_order_response_availability`·`sli_order_business_success`·`sli_cancel_success`
  세 Rate가 출력되고 값이 sane(전부 성공 저부하 → 가용성·업무 ~100%). **파싱·집계가 도는지**가
  스모크의 목적(과부하 셰딩 재현은 GCP 몫). backend 없으면 스모크는 GCP 세션으로 미루고 셀프체크(Task 1)로 대체.

- [x] **Step 5: Commit** — 초안: `feat(loadtest): 스파이크 스크립트에 SLI 3분할 Rate 추가 (3차 ③)`

---

### Task 3: 문서 + README

**Files:**
- Create: `docs/refactor/16_3차③_SLI_3분할_완료.md`
- Modify: `docs/refactor/README.md`(3차 ③ ✅)

- [x] **Step 1: 완료 문서** — `16_3차③_SLI_3분할_완료.md`: 왜(23번 "가용성 vs 업무 vs 취소"
  혼선) / 어떻게(3 Rate·판정표·RESPONSE_SLO_MS 1s·순수함수+셀프체크·whole-run·threshold 없음) /
  결과(셀프체크 PASS·스모크). **PromQL 유도 예시**(업무·취소 성공률을 기존
  `HTTPRequestsTotal{path,method,status}`에서 유도) 병기. **RESPONSE_SLO_MS는 벤치 경계 기본값이지
  프로덕션 SLO 선언 아님**을 명기.
- [x] **Step 2: README** — 3차 표 ③ 🔨→✅ + 완료 문서 링크.
- [x] **Step 3: Commit + 푸시 + CI** — author→reviewer. Go 무변경이라 CI는 회귀 없음 확인(`gh run watch`).

---

## 다음 (범위 밖)

① 취소 500 제거(emit 분리+백프레셔), ② BTC 처리량(pprof 진단 선행). ③의 3 SLI 위에서
①②의 효과를 세 지표로 정확히 잰다. Rate threshold(합격 목표)·스테이지 태깅·서버 SLI
대시보드는 재측정/필요 시 후속.
