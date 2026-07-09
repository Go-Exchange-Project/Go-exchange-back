# 스트레스 테스트 VU 상한 확장 설계

## 배경 (왜 필요한가)

`11-2026-07-09-vm-cpu-scaling-control.md`에서 VU 700+ 구간부터 CPU가 다시 93~95%까지 오르고 `order` 채널이 잠깐 포화되는 걸 관찰했다. 하지만 지금 `loadtest/order-submission-stress.js`의 `STRESS_STAGES`가 VU 800에서 램프업을 끝내도록 짜여 있어서, 이게 시스템의 진짜 한계인지 아니면 테스트 스크립트 자체가 거기서 멈추게 설정된 것뿐인지 구분이 안 된다.

실제 암호화폐 거래소는 평소엔 조용하다가 급등/급락 시점에 짧은 시간에 훨씬 많은 동시 접속이 몰린다 — VU 800은 이런 시나리오를 재현하기엔 작을 수 있다는 지적이 나왔다. 이번 작업은 VU 상한을 3,000까지 확장해서, 진짜 벽이 어디 있는지부터 다시 확인한다. 이 결과에 따라 다음 개선 방향(Postgres 분리 vs 매칭엔진 샤딩)의 우선순위를 정한다.

## 왜 이 방식을 선택했는지

기존 `STRESS_STAGES`가 50→100→200→400→800으로 대략 2배씩 늘어나는 패턴이라, 이 패턴을 그대로 이어서 1,600→3,000으로 확장했다. 방법론을 안 바꿔야 이전 결과(03~11번)와 계속 비교 가능하다.

VU 수만 늘리고 `TOTAL_USERS`(사전 등록 유저 수)를 그대로 두면, 여러 VU가 같은 유저 계정을 공유하게 돼서(`vuIndex = (exec.vu.idInTest - 1) % data.users.length`) 지갑 잠금 경합이라는 별개의 변수가 매칭엔진 부하 측정에 섞여 들어간다. 그래서 `TOTAL_USERS`도 VU 상한과 함께 3,000으로 올려서, 이 실험이 순수하게 "동시 주문량"만 측정하게 했다.

## 범위

- `loadtest/order-submission-stress.js`의 `STRESS_STAGES`, `TOTAL_USERS`, `setupTimeout`을 수정한다.
- 서버/인프라 코드는 변경하지 않는다 — k6 스크립트만 부하생성 인스턴스에 재배포하면 된다(백엔드 서버 재배포는 불필요).
- 실제 실행/결과 문서화는 이 스펙 문서화 이후 사용자와 직접 진행한다.

## 아키텍처

### 1. `STRESS_STAGES` 확장

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

다음과 같이 확장한다:

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

### 2. `TOTAL_USERS`와 `setupTimeout` 조정

현재:
```js
const TOTAL_USERS = 800;
```
```js
export const options = {
  setupTimeout: '5m',
  ...
};
```

다음과 같이 수정한다:
```js
const TOTAL_USERS = 3000;
```
```js
export const options = {
  // TOTAL_USERS=3000 sequential register-or-login + fund calls take well over
  // k6's default 60s setup() timeout, so this must be raised explicitly.
  setupTimeout: '10m',
  ...
};
```

(주석도 유저 수에 맞게 800→3000으로 갱신한다.)

### 3. 실행/검증 절차 (사용자와 직접 진행, 이 스펙의 범위 밖)

1. 수정된 스크립트를 부하생성 인스턴스(`goexchange-stress-load-gen`)에 업로드(`scp`).
2. 서버는 이미 `e2-highcpu-4`로 배포된 최신 코드(`a0c5483` 이후) 그대로 사용 — 재배포 불필요.
3. k6 재실행. VU 800 이후(1,600, 3,000 구간)에서 CPU, `matching_engine_channel_length`, `order_pipeline_match_latency_seconds` p95를 관찰.
4. 결과를 `docs/benchmarks/12-YYYY-MM-DD-stress-test-vu-ceiling-extension.md`에 기록 — 11번을 기준선으로, "진짜 벽이 어디였는지"를 명확히 결론짓는다.

## 성공 기준

- k6가 VU 3,000까지 문법/설정 에러 없이 램프업을 완주하거나(완주 못 해도 무엇이 먼저 무너지는지가 유의미한 데이터), 어느 지점에서 시스템이 한계에 도달하는지 명확한 지표(CPU, 채널 길이, 지연)로 뒷받침되는 결론을 얻는다.

## 범위 밖 (Out of Scope)

- 이번 결과에 따른 실제 인프라/코드 개선(Postgres 분리, 매칭엔진 샤딩) — 결과를 보고 별도로 브레인스토밍.
- `http_req_failed`의 소량 네트워크 레벨 실패 원인 조사 — 별도 작업으로 남겨둔다.
