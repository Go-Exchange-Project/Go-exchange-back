import http from 'k6/http';
import { check, sleep } from 'k6';
import exec from 'k6/execution';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const DEV_TOOLS_TOKEN = __ENV.DEV_TOOLS_TOKEN;
const DEV_TOOLS_TOKEN_HEADER = 'X-GoExchange-Dev-Token';

const TOTAL_USERS = 25000;
const SETUP_BATCH_SIZE = 100;
const COIN_SYMBOL = 'BTC';
const FIXED_PRICE = '50000000';
const ORDER_AMOUNT = '0.001';
const BUYER_KRW_FUNDING = '100000000';
const SELLER_BTC_FUNDING = '1000';

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

export function submitOrders(data) {
  const vuIndex = (exec.vu.idInTest - 1) % data.users.length;
  const user = data.users[vuIndex];

  const orderBody = {
    coin_symbol: COIN_SYMBOL,
    side: user.role === 'buyer' ? 'BUY' : 'SELL',
    order_type: 'LIMIT',
    price: FIXED_PRICE,
    amount: ORDER_AMOUNT,
  };

  const res = http.post(`${BASE_URL}/orders`, JSON.stringify(orderBody), {
    headers: {
      'Content-Type': 'application/json',
      Authorization: `Bearer ${user.token}`,
    },
    tags: { name: 'create_order' },
  });

  check(res, {
    'order accepted (status 200)': (r) => r.status === 200,
  });

  sleep(0.2 + Math.random() * 0.3);
}
