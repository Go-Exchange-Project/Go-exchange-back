import http from 'k6/http';
import { check, sleep } from 'k6';
import exec from 'k6/execution';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const DEV_TOOLS_TOKEN = __ENV.DEV_TOOLS_TOKEN;
const DEV_TOOLS_TOKEN_HEADER = 'X-GoExchange-Dev-Token';

const TOTAL_USERS = parseInt(__ENV.TOTAL_USERS || '3000', 10);
const SETUP_BATCH_SIZE = 100;
// 심볼별 등록 정책(config/market_rules.json)이 다르다 - XRP는 정수 단위
// (min_order_quantity=1, base_quantity_step=1)로 등록돼 있어 다른 심볼과 같은
// 소수점 수량(0.001)을 쓰면 주문이 거부된다. 심볼별 주문 수량을 분리한다.
const SYMBOLS = ['BTC', 'ETH', 'XRP', 'SOL', 'ADA', 'DOGE', 'TRX', 'DOT'];
const FIXED_PRICE = '50000000';
const ORDER_AMOUNT_BY_SYMBOL = {
  XRP: '1',
};
const DEFAULT_ORDER_AMOUNT = '0.001';
// XRP는 수량이 1000배 커서(0.001 -> 1) 명목가가 그만큼 크다. 두 경우 모두
// 부하 구간(9.5분) 동안 잔고가 바닥나지 않도록 넉넉히 충전한다.
const SELLER_COIN_FUNDING = '1000000';
const BUYER_KRW_FUNDING = '1000000000000';

function orderAmountFor(symbol) {
  return ORDER_AMOUNT_BY_SYMBOL[symbol] ?? DEFAULT_ORDER_AMOUNT;
}

// steady-state 포화 프로파일: 워밍업(0->TOTAL_USERS) 후 TOTAL_USERS에서 유지.
// 기본값은 GCP hold 프로파일(1.5분 워밍업 + 8분 유지)이고, 로컬 스모크 시
// WARMUP_DURATION/HOLD_DURATION을 짧게 오버라이드한다.
const HOLD_STAGES = [
  { duration: __ENV.WARMUP_DURATION || '1.5m', target: TOTAL_USERS },
  { duration: __ENV.HOLD_DURATION || '8m', target: TOTAL_USERS },
];

export const options = {
  setupTimeout: '20m',
  batch: SETUP_BATCH_SIZE,
  batchPerHost: SETUP_BATCH_SIZE,
  scenarios: {
    order_submission_hold: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: HOLD_STAGES,
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

    // 매수/매도 쌍이 같은 심볼이어야 체결이 난다: 연속된 두 유저(2k-1, 2k)가
    // 같은 심볼 버킷을 갖도록 pair index(0-based)를 심볼 수로 나눈 나머지로 배정한다.
    const fundRequests = batchIndices.map((i) => {
      const role = i % 2 === 1 ? 'buyer' : 'seller';
      const pairIndex = Math.floor((i - 1) / 2);
      const symbol = SYMBOLS[pairIndex % SYMBOLS.length];
      const fundBody =
        role === 'buyer'
          ? { coin_symbol: 'KRW', amount: BUYER_KRW_FUNDING }
          : { coin_symbol: symbol, amount: SELLER_COIN_FUNDING };
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
      const pairIndex = Math.floor((i - 1) / 2);
      const symbol = SYMBOLS[pairIndex % SYMBOLS.length];
      users.push({ token: tokensByIndex[i], role, symbol });
    });
  }

  return { users };
}

export function submitOrders(data) {
  const vuIndex = (exec.vu.idInTest - 1) % data.users.length;
  const user = data.users[vuIndex];

  const orderBody = {
    coin_symbol: user.symbol,
    side: user.role === 'buyer' ? 'BUY' : 'SELL',
    order_type: 'LIMIT',
    price: FIXED_PRICE,
    amount: orderAmountFor(user.symbol),
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
