import http from 'k6/http';
import { check, sleep } from 'k6';
import exec from 'k6/execution';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const DEV_TOOLS_TOKEN = __ENV.DEV_TOOLS_TOKEN;
const DEV_TOOLS_TOKEN_HEADER = 'X-GoExchange-Dev-Token';

const TOTAL_USERS = 25000;
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
  for (let i = 1; i <= TOTAL_USERS; i++) {
    // 홀/짝수로 역할을 배정해, VU ID가 낮은 구간(램프업 초반)에도 매수자/매도자가
    // 함께 활성화되도록 한다.
    const role = i % 2 === 1 ? 'buyer' : 'seller';
    const email = `stress-user-${i}@test.local`;

    // 가입-또는-로그인 폴백: 같은 테스트 DB에 스크립트를 여러 번 실행해도
    // 안전하게 재실행 가능하도록 한다.
    const registerRes = http.post(
      `${BASE_URL}/auth/register`,
      JSON.stringify({
        name: `Stress Test User ${i}`,
        email: email,
        password: 'loadtest-password-123',
      }),
      { headers: { 'Content-Type': 'application/json' }, tags: { name: 'setup' } }
    );

    let token;
    if (registerRes.status === 201) {
      token = registerRes.json('data.token');
    } else if (registerRes.status === 409) {
      const loginRes = http.post(
        `${BASE_URL}/auth/login`,
        JSON.stringify({ email: email, password: 'loadtest-password-123' }),
        { headers: { 'Content-Type': 'application/json' }, tags: { name: 'setup' } }
      );
      if (loginRes.status !== 200) {
        throw new Error(
          `setup: user ${i} (${email}) already registered but login failed: ${loginRes.status} ${loginRes.body}`
        );
      }
      token = loginRes.json('data.token');
    } else {
      throw new Error(
        `setup: failed to register user ${i} (${email}): ${registerRes.status} ${registerRes.body}`
      );
    }

    const fundBody =
      role === 'buyer'
        ? { coin_symbol: 'KRW', amount: BUYER_KRW_FUNDING }
        : { coin_symbol: COIN_SYMBOL, amount: SELLER_BTC_FUNDING };

    const fundRes = http.post(`${BASE_URL}/dev/wallets/fund`, JSON.stringify(fundBody), {
      headers: {
        'Content-Type': 'application/json',
        Authorization: `Bearer ${token}`,
        [DEV_TOOLS_TOKEN_HEADER]: DEV_TOOLS_TOKEN,
      },
      tags: { name: 'setup' },
    });

    if (fundRes.status !== 200) {
      throw new Error(
        `setup: failed to fund wallet for user ${i} (${email}): ${fundRes.status} ${fundRes.body}`
      );
    }

    users.push({ token, role });
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
