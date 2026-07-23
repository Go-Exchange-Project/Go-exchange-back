import http from 'k6/http';
import { check, sleep } from 'k6';
import exec from 'k6/execution';
import { Counter } from 'k6/metrics';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const DEV_TOOLS_TOKEN = __ENV.DEV_TOOLS_TOKEN;
const DEV_TOOLS_TOKEN_HEADER = 'X-GoExchange-Dev-Token';

// 23번(⑤): 22번 order-spike-single-symbol.js 계승. 목표 5,000 hold + 초과
// ~10,000 버스트를 담기 위해 TOTAL_USERS를 피크 VU(10,000)에 맞춘다(1 VU = 1 user,
// 지갑 공유로 인한 잔고 경합 혼선을 피한다 — 22번과 동일 원칙).
const TOTAL_USERS = parseInt(__ENV.TOTAL_USERS || '10000', 10);
const SETUP_BATCH_SIZE = 100;
const COIN_SYMBOL = 'BTC';

const BASE_PRICE = 50000000;
const TICK = 1000;
const ORDER_AMOUNT = '0.001';
const MARKET_BUY_QUOTE_AMOUNT = '50000';

const BUYER_KRW_FUNDING = '1000000000000';
const SELLER_BTC_FUNDING = '1000000';

const MAKER_RATIO = 0.6;
const MAKER_CANCEL_PROBABILITY = 0.3;
const TAKER_MARKET_RATIO = 0.5;

// 취소 응답의 404(주문 없음)·409(이미 체결)는 정상 레이스 결과 — 실패로 안 셈.
const cancelResponseCallback = http.expectedStatuses(200, 404, 409);
// 주문 생성의 503(입장 거절)은 ④의 의도된 "우아한 셰딩" — http_req_failed로 안 셈,
// custom_fast_reject_503로 별도 집계.
const orderResponseCallback = http.expectedStatuses(200, 201, 503);

const orderSuccess = new Counter('custom_order_success');
const orderFail = new Counter('custom_order_fail');
const marketSuccess = new Counter('custom_market_success');
const marketFail = new Counter('custom_market_fail');
const cancelSuccess = new Counter('custom_cancel_success');
const cancelAlreadyFilled = new Counter('custom_cancel_already_filled');
const cancelFail = new Counter('custom_cancel_fail');
const fastReject503 = new Counter('custom_fast_reject_503');
const rejectMissingRetryAfter = new Counter('custom_reject_missing_retry_after');

// ⑤ 확정 프로필: 300 웜(1분) → 5,000 램프(30초) → 5,000 hold(3분, 목표) →
// ~10,000 버스트(45초, 초과) → 300 급락(30초) → 회복(2분). 로컬 스모크에서는
// STAGEn_DURATION/STAGEn_VUS로 오버라이드.
const SPIKE_STAGES = [
  { duration: __ENV.STAGE1_DURATION || '1m', target: parseInt(__ENV.STAGE1_VUS || '300', 10) },
  { duration: __ENV.STAGE2_DURATION || '30s', target: parseInt(__ENV.STAGE2_VUS || '5000', 10) },
  { duration: __ENV.STAGE3_DURATION || '3m', target: parseInt(__ENV.STAGE3_VUS || '5000', 10) },
  { duration: __ENV.STAGE4_DURATION || '45s', target: parseInt(__ENV.STAGE4_VUS || '10000', 10) },
  { duration: __ENV.STAGE5_DURATION || '30s', target: parseInt(__ENV.STAGE5_VUS || '300', 10) },
  { duration: __ENV.STAGE6_DURATION || '2m', target: parseInt(__ENV.STAGE6_VUS || '300', 10) },
];

export const options = {
  setupTimeout: '25m',
  batch: SETUP_BATCH_SIZE,
  batchPerHost: SETUP_BATCH_SIZE,
  scenarios: {
    order_spike_availability: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: SPIKE_STAGES,
      exec: 'runVU',
    },
  },
  // 판정 실패로 런을 중단시키지 않는다 — 스파이크 전체를 완주시켜 전부 기록.
  thresholds: {},
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
        name: `Spike Test User ${i}`,
        email: `spike-user-${i}@test.local`,
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
        JSON.stringify({ email: `spike-user-${i}@test.local`, password: 'loadtest-password-123' }),
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

function authHeaders(token) {
  return { 'Content-Type': 'application/json', Authorization: `Bearer ${token}` };
}

// 90초 타임아웃: A(옛 코드)는 초과 버스트에서 매달릴 것으로 예상되므로, k6 기본
// 60초보다 넉넉히 잡아 진짜 tail latency를 k6 타임아웃이 아니라 서버 응답으로
// 관측한다.
function submitOrder(user, body) {
  const res = http.post(`${BASE_URL}/orders`, JSON.stringify(body), {
    headers: authHeaders(user.token),
    tags: { name: 'create_order' },
    responseCallback: orderResponseCallback,
    timeout: '90s',
  });
  check(res, {
    'order accepted or gracefully rejected': (r) => r.status === 200 || r.status === 201 || r.status === 503,
  });
  if (res.status === 503) {
    fastReject503.add(1);
    const retryAfterHeader = res.headers['Retry-After'];
    if (!retryAfterHeader) {
      rejectMissingRetryAfter.add(1);
    }
    // Retry-After를 실제로 존중해 백오프한다 — 헤더 존재 여부만 세고 무시하면
    // 클라이언트가 즉시 재시도하는 retry storm을 만들어 ④가 막으려는 문제를
    // 스스로 재현하게 된다. 헤더 값(초) + 지터로 다음 제출을 늦춘다.
    const retryAfterSeconds = parseFloat(retryAfterHeader) || 1;
    sleep(retryAfterSeconds + Math.random() * 0.5);
  }
  return res;
}

function makerFlow(user) {
  const offsetTicks = 1 + Math.floor(Math.random() * 5);
  const price =
    user.role === 'buyer' ? BASE_PRICE - offsetTicks * TICK : BASE_PRICE + offsetTicks * TICK;

  const res = submitOrder(user, {
    coin_symbol: COIN_SYMBOL,
    side: user.role === 'buyer' ? 'BUY' : 'SELL',
    order_type: 'LIMIT',
    price: String(price),
    amount: ORDER_AMOUNT,
  });

  if (res.status !== 200 && res.status !== 201) {
    orderFail.add(1);
    return;
  }
  orderSuccess.add(1);

  if (Math.random() >= MAKER_CANCEL_PROBABILITY) return;

  const orderId = res.json('data.order_id');
  sleep(1 + Math.random() * 2);

  const cancelRes = http.del(`${BASE_URL}/orders/${orderId}`, null, {
    headers: authHeaders(user.token),
    tags: { name: 'cancel_order' },
    responseCallback: cancelResponseCallback,
  });
  if (cancelRes.status === 200) {
    cancelSuccess.add(1);
  } else if (cancelRes.status === 404 || cancelRes.status === 409) {
    cancelAlreadyFilled.add(1);
  } else {
    cancelFail.add(1);
  }
}

function takerMarketFlow(user) {
  const body =
    user.role === 'buyer'
      ? { coin_symbol: COIN_SYMBOL, side: 'BUY', order_type: 'MARKET', quote_amount: MARKET_BUY_QUOTE_AMOUNT }
      : { coin_symbol: COIN_SYMBOL, side: 'SELL', order_type: 'MARKET', amount: ORDER_AMOUNT };

  const res = submitOrder(user, body);
  if (res.status === 200 || res.status === 201) {
    orderSuccess.add(1);
    marketSuccess.add(1);
  } else {
    orderFail.add(1);
    marketFail.add(1);
  }
}

function takerCrossingLimitFlow(user) {
  const price = user.role === 'buyer' ? BASE_PRICE + 10 * TICK : BASE_PRICE - 10 * TICK;
  const res = submitOrder(user, {
    coin_symbol: COIN_SYMBOL,
    side: user.role === 'buyer' ? 'BUY' : 'SELL',
    order_type: 'LIMIT',
    price: String(price),
    amount: ORDER_AMOUNT,
  });
  if (res.status === 200 || res.status === 201) {
    orderSuccess.add(1);
  } else {
    orderFail.add(1);
  }
}

export function runVU(data) {
  const vuIndex = (exec.vu.idInTest - 1) % data.users.length;
  const user = data.users[vuIndex];

  if (Math.random() < MAKER_RATIO) {
    makerFlow(user);
  } else if (Math.random() < TAKER_MARKET_RATIO) {
    takerMarketFlow(user);
  } else {
    takerCrossingLimitFlow(user);
  }

  sleep(0.2 + Math.random() * 0.3);
}
