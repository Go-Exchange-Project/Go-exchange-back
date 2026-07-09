# 스트레스 테스트 VU 상한 확장 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** k6 스트레스 테스트의 VU 상한을 800에서 3,000까지 확장해서, VM 코어 증설(11번 테스트) 이후 진짜 시스템 한계가 어디인지 재측정할 수 있게 한다.

**Architecture:** `loadtest/order-submission-stress.js`의 `STRESS_STAGES` 배열에 1,600/3,000 단계를 추가하고, `TOTAL_USERS`와 `setupTimeout`을 그에 맞게 늘린다. 서버 코드는 변경하지 않는다.

**Tech Stack:** k6 (JavaScript 스크립트).

## Global Constraints

- `STRESS_STAGES`는 기존 패턴(대략 2배씩 증가, 각 단계 2분)을 이어서 `1,600`, `3,000` 단계를 추가한다.
- `TOTAL_USERS`는 `3000`으로, `setupTimeout`은 `'10m'`으로 변경한다.
- 서버(`cmd/`, `internal/`) 코드는 이번 변경 대상이 아니다.

---

### Task 1: `loadtest/order-submission-stress.js` VU 상한 확장

**Files:**
- Modify: `loadtest/order-submission-stress.js`

**Interfaces:**
- 없음 (k6 스크립트 설정값만 변경, 다른 태스크가 이어지지 않음 — 단일 태스크 계획).

- [ ] **Step 1: `STRESS_STAGES` 확장**

`loadtest/order-submission-stress.js`의 현재:

```js
const STRESS_STAGES = [
  { duration: '2m', target: 50 },
  { duration: '2m', target: 100 },
  { duration: '2m', target: 200 },
  { duration: '2m', target: 400 },
  { duration: '2m', target: 800 },
];
```

다음과 같이 수정한다:

```js
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

- [ ] **Step 2: `TOTAL_USERS` 변경**

현재:

```js
const TOTAL_USERS = 800;
```

다음과 같이 수정한다:

```js
const TOTAL_USERS = 3000;
```

- [ ] **Step 3: `setupTimeout` 및 관련 주석 변경**

현재:

```js
export const options = {
  // TOTAL_USERS=800 sequential register-or-login + fund calls take well over
  // k6's default 60s setup() timeout, so this must be raised explicitly.
  setupTimeout: '5m',
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

다음과 같이 수정한다:

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

- [ ] **Step 4: 문법 검증**

```bash
node --check loadtest/order-submission-stress.js
```

Expected: 에러 없이 종료(k6 스크립트가 유효한 JS 문법인지 확인 — k6 자체는 이 저장소 CI에 없으므로 Node의 문법 검사로 대체).

- [ ] **Step 5: 커밋**

```bash
git add loadtest/order-submission-stress.js
git commit -m "$(cat <<'MSG'
feat: 스트레스 테스트 VU 상한을 800에서 3000으로 확장

VM 코어 증설(11번 테스트) 이후 VU 800이 시스템의 진짜 한계인지
스크립트 설정일 뿐인지 구분하기 위해, STRESS_STAGES에 1600/3000
단계를 추가하고 TOTAL_USERS/setupTimeout을 그에 맞게 늘린다.
MSG
)"
```
