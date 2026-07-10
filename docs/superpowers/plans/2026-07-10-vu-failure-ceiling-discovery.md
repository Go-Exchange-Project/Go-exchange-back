# VU 실패 상한 탐색용 부하테스트 스크립트 확장 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `loadtest/order-submission-stress.js`를 VU 25,000까지 램프업 가능하도록 확장하고, 그 규모에 맞춰 사전 등록 유저 수와 `setup()` 등록 로직을 조정한다.

**Architecture:** 단일 파일(`loadtest/order-submission-stress.js`) 수정. (1) 램프업 단계/유저 수/타임아웃 상수 확장, (2) `setup()`을 유저별 순차 요청에서 `SETUP_BATCH_SIZE`(100)명 단위 `http.batch()` 병렬 요청으로 재작성. 두 변경 모두 실제 GCP 인프라 배포·실행은 이 계획의 범위 밖(사용자와 직접 진행)이며, 로컬에서는 k6 CLI의 문법 검증과 백엔드 없는 드라이런으로 검증한다.

**Tech Stack:** k6 (JavaScript 기반 부하테스트 도구), `k6/http`의 `http.batch()`.

## Global Constraints

- `STRESS_STAGES`는 각 2분(`2m`)씩, `50 → 100 → 200 → 400 → 800 → 1600 → 3000 → 6000 → 12000 → 25000` 순서를 유지한다.
- `TOTAL_USERS = 25000`.
- `setupTimeout: '20m'`.
- `SETUP_BATCH_SIZE = 100` (배치당 동시 요청 유저 수).
- 유저 역할 배정 규칙(`i % 2 === 1` → buyer, 짝수 → seller)과 이메일 형식(`stress-user-${i}@test.local`), 비밀번호(`loadtest-password-123`), 펀딩 금액(`BUYER_KRW_FUNDING`, `SELLER_BTC_FUNDING`)은 기존 값 그대로 유지한다 — 변경하지 않는다.
- 이 계획은 `loadtest/order-submission-stress.js` 한 파일만 수정한다. 서버/인프라 코드는 건드리지 않는다.
- 실제 GCP 배포·k6 실행·실패 지점 관찰·`docs/benchmarks/17-...md` 문서화는 이 계획의 범위 밖이다(스펙 문서 참고).

---

### Task 1: 램프업 단계·유저 수·타임아웃 확장

**Files:**
- Modify: `loadtest/order-submission-stress.js:9` (`TOTAL_USERS`), `loadtest/order-submission-stress.js:16-27` (`STRESS_STAGES`와 그 위 주석), `loadtest/order-submission-stress.js:29-41` (`options.setupTimeout`)

**Interfaces:**
- Consumes: 없음 (이 파일의 최상단 상수만 변경).
- Produces: `TOTAL_USERS`(25000), `STRESS_STAGES`(10단계 배열), `options.setupTimeout`('20m') — Task 2의 `setup()`이 `TOTAL_USERS`를 그대로 사용한다.

- [ ] **Step 1: `TOTAL_USERS`를 25000으로 변경**

`loadtest/order-submission-stress.js:9`의 현재 코드:
```js
const TOTAL_USERS = 3000;
```
다음으로 교체:
```js
const TOTAL_USERS = 25000;
```

- [ ] **Step 2: `STRESS_STAGES` 배열과 주석을 확장**

`loadtest/order-submission-stress.js:16-27`의 현재 코드:
```js
// 사전에 정의된 목표 수치나 임계값 없이, Grafana 대시보드를 보면서 에러율 급증이나
// p95 급격 저하가 관측되는 시점에 수동으로 실행을 중단한다. 3000 VU까지도 시스템이
// 버틴다면 이 배열에 단계를 더 추가해서 계속 밀어붙일 수 있다.
const STRESS_STAGES = [
  { duration: '2m', target: 50 },
  { duration: '2m', target: 100 },
  { duration: '2m', target: 200 },
  { duration: '2m', target: 400 },
  { duration: '2m', target: 800 },
  { duration: '2m', target: 1600 },
  { duration: '2m', target: 3000 },
];
```
다음으로 교체:
```js
// 사전에 정의된 목표 수치나 임계값 없이, Grafana 대시보드를 보면서 에러율 급증이나
// p95 급격 저하가 관측되는 시점에 수동으로 실행을 중단한다. 25000 VU까지도 시스템이
// 버틴다면 이 배열에 단계를 더 추가해서 계속 밀어붙일 수 있다.
const STRESS_STAGES = [
  { duration: '2m', target: 50 },
  { duration: '2m', target: 100 },
  { duration: '2m', target: 200 },
  { duration: '2m', target: 400 },
  { duration: '2m', target: 800 },
  { duration: '2m', target: 1600 },
  { duration: '2m', target: 3000 },
  { duration: '2m', target: 6000 },
  { duration: '2m', target: 12000 },
  { duration: '2m', target: 25000 },
];
```

- [ ] **Step 3: `setupTimeout`을 20분으로 확장**

`loadtest/order-submission-stress.js:29-41`의 현재 코드:
```js
export const options = {
  // TOTAL_USERS=3000 sequential register-or-login + fund calls take well over
  // k6's default 60s setup() timeout, so this must be raised explicitly.
  setupTimeout: '10m',
  scenarios: {
    order_submission_stress: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: STRESS_STAGES,
      exec: 'submitOrders',
    },
  },
};
```
다음으로 교체:
```js
export const options = {
  // TOTAL_USERS=25000, http.batch()로 병렬화해도 250개 배치(SETUP_BATCH_SIZE=100)를
  // 순차 실행하므로 k6 기본 60초를 훨씬 초과한다.
  setupTimeout: '20m',
  scenarios: {
    order_submission_stress: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: STRESS_STAGES,
      exec: 'submitOrders',
    },
  },
};
```

- [ ] **Step 4: k6 문법 검증**

Run: `k6 inspect loadtest/order-submission-stress.js`
Expected: JSON 메타데이터가 에러 없이 출력되고, 그 안의 `"scenarios"` 값에 `"target": 25000`이 포함되어 있음. `k6 inspect`가 스크립트를 파싱·평가할 수 없으면(문법 에러 등) 0이 아닌 종료 코드와 함께 실패 메시지를 출력한다.

- [ ] **Step 5: Commit**

```bash
git add loadtest/order-submission-stress.js
git commit -m "test: 부하테스트 VU 상한을 25000까지 확장"
```

---

### Task 2: `setup()`을 `http.batch()` 기반 배치 병렬 등록으로 재작성

**Files:**
- Modify: `loadtest/order-submission-stress.js:43-114` (`setup()` 함수 전체)

**Interfaces:**
- Consumes: Task 1에서 정한 `TOTAL_USERS`(25000), 모듈 상단의 `BASE_URL`, `DEV_TOOLS_TOKEN`, `DEV_TOOLS_TOKEN_HEADER`, `COIN_SYMBOL`, `BUYER_KRW_FUNDING`, `SELLER_BTC_FUNDING` (기존 그대로, 변경 없음).
- Produces: `setup()`은 기존과 동일하게 `{ users: [{ token, role }, ...] }` 형태의 객체를 반환한다 — `submitOrders(data)`(`loadtest/order-submission-stress.js:116-141`)가 `data.users`를 그대로 소비하므로 반환 형태를 바꾸면 안 된다.

- [ ] **Step 1: `SETUP_BATCH_SIZE` 상수 추가**

`loadtest/order-submission-stress.js:9` 바로 아래(`TOTAL_USERS` 다음 줄)에 추가:
```js
const SETUP_BATCH_SIZE = 100;
```

- [ ] **Step 2: `setup()` 함수를 배치 병렬 버전으로 교체**

`loadtest/order-submission-stress.js:43-114`의 현재 `setup()` 전체(유저 1명씩 순차 `register`→`login` 폴백→`fund`)를 다음으로 교체:

```js
export function setup() {
  if (!DEV_TOOLS_TOKEN) {
    throw new Error(
      'DEV_TOOLS_TOKEN environment variable is required (pass -e DEV_TOOLS_TOKEN=<value> matching the server\'s GOEXCHANGE_DEV_TOOLS_TOKEN)'
    );
  }

  const users = [];
  for (let batchStart = 1; batchStart <= TOTAL_USERS; batchStart += SETUP_BATCH_SIZE) {
    const batchEnd = Math.min(batchStart + SETUP_BATCH_SIZE - 1, TOTAL_USERS);
    const batchIndices = [];
    for (let i = batchStart; i <= batchEnd; i++) batchIndices.push(i);

    // 1단계: 이 배치의 등록 요청을 동시에 전송
    const registerRequests = batchIndices.map((i) => [
      'POST',
      `${BASE_URL}/auth/register`,
      JSON.stringify({
        name: `Stress Test User ${i}`,
        email: `stress-user-${i}@test.local`,
        password: 'loadtest-password-123',
      }),
      { headers: { 'Content-Type': 'application/json' }, tags: { name: 'setup' } },
    ]);
    const registerResponses = http.batch(registerRequests);

    // 2단계: 409(이미 등록됨)인 유저만 모아 로그인 요청을 동시에 전송
    const loginNeeded = [];
    const tokensByIndex = {};
    registerResponses.forEach((res, idx) => {
      const i = batchIndices[idx];
      if (res.status === 201) {
        tokensByIndex[i] = res.json('data.token');
      } else if (res.status === 409) {
        loginNeeded.push(i);
      } else {
        throw new Error(`setup: failed to register user ${i}: ${res.status} ${res.body}`);
      }
    });

    if (loginNeeded.length > 0) {
      const loginRequests = loginNeeded.map((i) => [
        'POST',
        `${BASE_URL}/auth/login`,
        JSON.stringify({ email: `stress-user-${i}@test.local`, password: 'loadtest-password-123' }),
        { headers: { 'Content-Type': 'application/json' }, tags: { name: 'setup' } },
      ]);
      const loginResponses = http.batch(loginRequests);
      loginResponses.forEach((res, idx) => {
        const i = loginNeeded[idx];
        if (res.status !== 200) {
          throw new Error(`setup: user ${i} already registered but login failed: ${res.status} ${res.body}`);
        }
        tokensByIndex[i] = res.json('data.token');
      });
    }

    // 3단계: 이 배치의 지갑 충전 요청을 동시에 전송
    const fundRequests = batchIndices.map((i) => {
      const role = i % 2 === 1 ? 'buyer' : 'seller';
      const fundBody =
        role === 'buyer'
          ? { coin_symbol: 'KRW', amount: BUYER_KRW_FUNDING }
          : { coin_symbol: COIN_SYMBOL, amount: SELLER_BTC_FUNDING };
      return [
        'POST',
        `${BASE_URL}/dev/wallets/fund`,
        JSON.stringify(fundBody),
        {
          headers: {
            'Content-Type': 'application/json',
            Authorization: `Bearer ${tokensByIndex[i]}`,
            [DEV_TOOLS_TOKEN_HEADER]: DEV_TOOLS_TOKEN,
          },
          tags: { name: 'setup' },
        },
      ];
    });
    const fundResponses = http.batch(fundRequests);
    fundResponses.forEach((res, idx) => {
      const i = batchIndices[idx];
      if (res.status !== 200) {
        throw new Error(`setup: failed to fund wallet for user ${i}: ${res.status} ${res.body}`);
      }
      const role = i % 2 === 1 ? 'buyer' : 'seller';
      users.push({ token: tokensByIndex[i], role });
    });
  }

  return { users };
}
```

- [ ] **Step 3: k6 문법 검증**

Run: `k6 inspect loadtest/order-submission-stress.js`
Expected: Task 1의 Step 4와 동일하게 JSON 메타데이터가 에러 없이 출력됨. 이 검증은 문법(파싱) 오류만 잡으며, `setup()` 내부 로직은 실행하지 않는다 — 실제 로직 검증은 다음 Step에서 한다.

- [ ] **Step 4: 백엔드 없이 로컬 드라이런으로 배치 로직 검증**

실제 백엔드 서버 없이, 응답을 절대 받을 수 없는 주소(`http://localhost:19999`)에 요청을 보내 `http.batch()` 코드 경로 자체가 문법·런타임 에러 없이 실행되는지 확인한다. 연결이 거부되면 `http.batch()`가 던지지 않고 `status: 0`인 응답 객체를 반환하므로, 위 코드의 `else { throw new Error(...) }` 분기가 실행되어 아래와 같은 **제어된 에러**로 실패해야 한다(스크립트 자체의 문법/런타임 버그가 아님을 확인하는 것이 목적).

Run:
```bash
k6 run --vus 1 --iterations 1 -e BASE_URL=http://localhost:19999 -e DEV_TOOLS_TOKEN=dryrun-token loadtest/order-submission-stress.js
```
Expected: k6 실행이 `setup()` 단계에서 실패로 종료되고, 에러 메시지에 `setup: failed to register user 1: 0` 문자열이 포함된다(`status 0`은 연결 실패를 의미). `TypeError`, `is not a function`, `is not defined` 등 JavaScript 런타임 에러가 아니라 위에서 명시적으로 `throw`한 `Error` 메시지여야 한다 — 그런 JS 런타임 에러가 나오면 `http.batch()` 호출 형식이나 응답 처리 로직에 버그가 있다는 뜻이므로 코드를 다시 확인한다.

- [ ] **Step 5: Commit**

```bash
git add loadtest/order-submission-stress.js
git commit -m "test: 부하테스트 setup()을 http.batch() 기반 병렬 등록으로 재작성"
```
