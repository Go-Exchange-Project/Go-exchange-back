# 인프라 증설 전, 현재 사양의 진짜 실패 VU 확인 설계

## 배경 (왜 필요한가)

지금까지 확인된 한계는 두 가지로 나뉜다.

1. **부하생성기(`e2-standard-2`, 2 vCPU/8GB)가 만들어낼 수 있는 VU 자체의 한계**: `e2-small`(2GB)로는 VU~584에서 k6 프로세스가 OOM으로 죽었지만(12번 테스트), `e2-standard-2`로 올린 뒤로는 VU 3,000까지는 완주가 확인됐다 — **그 이상은 시도한 적이 없어 부하생성기의 진짜 상한을 모른다.**
2. **백엔드(서버+DB)가 "잘" 처리하는 성능 한계**: CPU가 VU 700~800 부근에서 90%로 포화되고, 그 이후로는 VU를 3,000까지 올려도 CPU는 안 오르지만 큐잉 지연(p95)이 계속 쌓이기만 한다(12번 테스트). VU 3,000까지는 크래시하지 않았다.

실제 거래소는 급등/급락 시 짧은 시간에 훨씬 많은 동시 접속(사용자가 언급한 "몇 십만" 수준, 이번 실험 목표는 1만~3만)이 몰릴 수 있다. 이 규모를 재현하려면 부하생성기/서버 인프라를 증설해야 할 가능성이 높지만, **증설하기 전에 지금 사양(서버 `e2-highcpu-4`, DB `e2-highcpu-4`, 부하생성기 `e2-standard-2`)으로 정확히 어디서부터 실패하는지부터 실측**한다. 이 결과가 다음 결정(부하생성기만 올릴지, 분산 부하생성이 필요한지, 백엔드도 더 올려야 하는지)의 근거가 된다.

## 왜 이 방식을 선택했는지

- 기존 `STRESS_STAGES`가 대략 2배씩 늘어나는 패턴(50→100→200→400→800→1,600→3,000)이라 이 패턴을 그대로 이어 3,000 이후로도 2배씩(6,000→12,000→25,000) 확장한다 — 방법론을 바꾸지 않아야 이전 결과(07~16번)와 계속 비교 가능하다.
- VU 상한을 올리면서 `TOTAL_USERS`(사전 등록 유저 수)를 그대로 두면 여러 VU가 같은 유저 계정·같은 지갑을 공유하게 된다(`vuIndex = (exec.vu.idInTest - 1) % data.users.length`). 이러면 지갑 락(`FOR UPDATE`) 경합이라는 별개의 변수가 "동시 접속자 수" 측정에 섞여 들어간다. 12번 테스트에서 정한 원칙(VU와 유저 수를 함께 늘려서 순수 동시성만 측정)을 그대로 유지해, `TOTAL_USERS`도 25,000으로 함께 올린다.
- `TOTAL_USERS`가 3,000→25,000(약 8.3배)이 되면, 지금처럼 유저 1명씩 순차 등록(`register` 또는 `login` 폴백 → `fund`)하는 `setup()`은 실행 시간이 비례해서 늘어나 1시간을 넘길 수 있다. k6의 `http.batch()`로 여러 유저의 등록/충전 요청을 동시에 묶어 보내면 벽시계 시간을 크게 줄일 수 있어 이 방식을 택한다.
- 지금까지는 "에러율 급증이나 p95 급격 저하가 관측되면 사람이 Grafana를 보고 수동으로 중단"하는 방식이었다. 이번엔 VU 25,000까지 자동으로 램프업하는 무인 실행이 될 수 있으므로, 사람이 안 지켜봐도 "실패"를 판단할 수 있는 명시적 기준이 필요하다.

## 범위

- `loadtest/order-submission-stress.js`의 `STRESS_STAGES`, `TOTAL_USERS`, `setup()` 로직(병렬 배치화), `setupTimeout`을 수정한다.
- 서버/DB 인프라 사양은 변경하지 않는다 — 이번 실험의 목적 자체가 "증설 없이 현재 사양의 한계를 찾는 것"이다.
- k6 스크립트에 실패 판정 임계값(에러율 급증/OOM/응답불능, p95 30초)을 코드 또는 실행 시 관찰 기준으로 명시한다.
- 실제 실행/결과 문서화는 스펙 작성 이후 사용자와 직접 진행한다(이 스펙의 범위 밖).

## 아키텍처

### 1. `STRESS_STAGES` 확장

현재:
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
  { duration: '2m', target: 6000 },
  { duration: '2m', target: 12000 },
  { duration: '2m', target: 25000 },
];
```

### 2. `TOTAL_USERS` 확장과 `setup()` 병렬화

현재:
```js
const TOTAL_USERS = 3000;
```

다음과 같이 수정한다:
```js
const TOTAL_USERS = 25000;
```

현재 `setup()`은 유저 1명당 순차로 `register`(또는 409 시 `login` 폴백) → `fund` 요청을 보낸다. 25,000명을 이 방식 그대로 돌리면 3,000명 기준 최대 10분이 걸렸던 게 8배 이상으로 늘어나 비현실적이다. 유저 간에는 등록 순서 의존성이 없으므로(유저 내부에서만 `register`→`fund` 순서 의존), 유저 단위로 묶어 `http.batch()`로 동시 전송한다:

```js
const SETUP_BATCH_SIZE = 100;

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

`SETUP_BATCH_SIZE`(100)는 부하생성기 자체가 setup 단계에서 과부하로 죽지 않도록 하는 값으로, 실행 중 부하생성기 리소스가 빠듯하면 실행 계획 단계에서 조정한다.

### 3. `setupTimeout` 조정

병렬화로 벽시계 시간이 크게 줄어들지만, 25,000명 규모이므로 여유를 두어 상향한다:
```js
export const options = {
  // TOTAL_USERS=25000, http.batch()로 병렬화해도 250개 배치(SETUP_BATCH_SIZE=100)를
  // 순차 실행하므로 k6 기본 60초를 훨씬 초과한다.
  setupTimeout: '20m',
  ...
};
```

### 4. 실패 판정 기준 (실행 시 관찰 기준, 자동 assert는 아님)

다음 중 하나가 관측되면 그 시점의 VU를 "실패 지점"으로 기록하고 실행을 중단한다:

- `http_req_failed` 급증 (예: 갑자기 5% 이상으로 튐)
- 부하생성기 프로세스가 OOM 등으로 죽음(`dmesg`/k6 로그 확인)
- 서버가 응답 불능(연결 거부/타임아웃 폭증)이 됨
- `http_req_duration` p95가 **30초**를 초과함

기존처럼 Grafana를 보며 사람이 판단하되, 위 네 가지를 명시적 체크리스트로 삼는다. 단순히 느려지기만 하고 위 기준에 안 걸리면(12번 테스트의 VU 3,000처럼) "실패"로 기록하지 않고 계속 진행한다.

### 5. 실행/검증 절차 (사용자와 직접 진행, 이 스펙의 범위 밖)

1. 수정된 스크립트를 부하생성 인스턴스(`goexchange-stress-load-gen`)에 업로드.
2. 서버/DB는 이미 배포된 `e2-highcpu-4`/`e2-highcpu-4` 사양 그대로 사용 — 재배포 불필요.
3. k6 실행. Grafana로 CPU, 채널 길이, 매칭 지연, `http_req_failed`, p95를 관찰하며 위 실패 기준 중 하나에 걸리는 시점(VU)을 기록.
4. 결과를 `docs/benchmarks/17-YYYY-MM-DD-vu-failure-ceiling-discovery.md`에 기록 — 어떤 컴포넌트(부하생성기 vs 서버 vs DB)가 먼저 무너졌는지, 몇 VU였는지 명확히 결론짓는다.

## 성공 기준

VU 25,000까지 도달하거나, 그 전에 위 실패 기준 중 하나에 걸리는 시점을 명확한 지표(CPU, 채널 길이, `http_req_failed`, p95, 부하생성기 리소스)로 뒷받침되는 결론으로 기록한다. "부하생성기가 먼저 죽었다" / "서버가 먼저 느려졌다" / "25,000까지도 안 죽었다" 중 어느 결론이든 유의미하다.

## 범위 밖 (Out of Scope)

- 이번 결과에 따른 실제 인프라 증설(부하생성기 스케일업, 분산 부하생성, 서버/DB 추가 증설) — 결과를 보고 별도로 브레인스토밍.
- `http_req_failed`의 기존에 관측되던 소량(0.4~1.3%) 네트워크 레벨 실패 원인 조사 — 별도 작업으로 남겨둔다(이번 스펙에서 정의한 "급증" 기준과는 별개).
