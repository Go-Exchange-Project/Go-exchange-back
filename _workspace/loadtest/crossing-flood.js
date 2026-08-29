import http from 'k6/http';
import { sleep } from 'k6';
import { buildIdempotencyKey } from './idempotency-key.js';

// 3차②-진단: 최초 바인딩 링크 진단 전용 드라이버. 앱 코드 아님 — 측정 도구.
// 단일 심볼 BTC에 crossing 주문(상대편을 항상 넘는 가격)만 계속 쏴서 체결을
// 최대화한다. maker 대기·취소는 없음 — 순수 체결 생성 부하.
const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const DEV_TOOLS_TOKEN = __ENV.DEV_TOOLS_TOKEN;
const DEV_TOOLS_TOKEN_HEADER = 'X-GoExchange-Dev-Token';

const TOTAL_USERS = parseInt(__ENV.TOTAL_USERS || '400', 10);
const VUS = parseInt(__ENV.VUS || '400', 10);
const DURATION = __ENV.DURATION || '120s';
const SLEEP_MS = parseFloat(__ENV.SLEEP_MS || '20') / 1000;

const COIN_SYMBOL = 'BTC';
const BASE_PRICE = 50000000;
const TICK = 1000;
const ORDER_AMOUNT = '0.001';
const BUYER_KRW_FUNDING = '1000000000000';
const SELLER_BTC_FUNDING = '1000000';

// 202는 멱등성 계약의 PENDING(주문·hold는 커밋됨) — 실패로 세면 안 된다.
const orderResponseCallback = http.expectedStatuses(200, 201, 202, 503);

export const options = {
  setupTimeout: '10m',
  batch: 100,
  batchPerHost: 100,
  scenarios: {
    crossing_flood: {
      executor: 'constant-vus',
      vus: VUS,
      duration: DURATION,
      exec: 'runVU',
    },
  },
  thresholds: {},
};

export function setup() {
  if (!DEV_TOOLS_TOKEN) {
    throw new Error('DEV_TOOLS_TOKEN environment variable is required');
  }
  const users = [];
  const BATCH = 100;
  for (let start = 1; start <= TOTAL_USERS; start += BATCH) {
    const end = Math.min(start + BATCH - 1, TOTAL_USERS);
    const idx = [];
    for (let i = start; i <= end; i++) idx.push(i);

    const registerReqs = idx.map((i) => [
      'POST',
      `${BASE_URL}/auth/register`,
      JSON.stringify({ name: `Diag24 User ${i}`, email: `diag24-user-${i}@test.local`, password: 'diag-password-123' }),
      { headers: { 'Content-Type': 'application/json' }, tags: { name: 'setup' } },
    ]);
    const registerRes = http.batch(registerReqs);
    const tokensByIdx = {};
    const loginNeeded = [];
    registerRes.forEach((res, j) => {
      const i = idx[j];
      if (res.status === 201) tokensByIdx[i] = res.json('data.token');
      else if (res.status === 409) loginNeeded.push(i);
      else throw new Error(`setup register failed for ${i}: ${res.status} ${res.body}`);
    });
    if (loginNeeded.length > 0) {
      const loginReqs = loginNeeded.map((i) => [
        'POST',
        `${BASE_URL}/auth/login`,
        JSON.stringify({ email: `diag24-user-${i}@test.local`, password: 'diag-password-123' }),
        { headers: { 'Content-Type': 'application/json' }, tags: { name: 'setup' } },
      ]);
      const loginRes = http.batch(loginReqs);
      loginRes.forEach((res, j) => {
        const i = loginNeeded[j];
        if (res.status !== 200) throw new Error(`setup login failed for ${i}: ${res.status} ${res.body}`);
        tokensByIdx[i] = res.json('data.token');
      });
    }
    const fundReqs = idx.map((i) => {
      const role = i % 2 === 1 ? 'buyer' : 'seller';
      const body = role === 'buyer' ? { coin_symbol: 'KRW', amount: BUYER_KRW_FUNDING } : { coin_symbol: COIN_SYMBOL, amount: SELLER_BTC_FUNDING };
      return [
        'POST',
        `${BASE_URL}/dev/wallets/fund`,
        JSON.stringify(body),
        { headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${tokensByIdx[i]}`, [DEV_TOOLS_TOKEN_HEADER]: DEV_TOOLS_TOKEN }, tags: { name: 'setup' } },
      ];
    });
    const fundRes = http.batch(fundReqs);
    fundRes.forEach((res, j) => {
      const i = idx[j];
      if (res.status !== 200) throw new Error(`setup fund failed for ${i}: ${res.status} ${res.body}`);
      users.push({ token: tokensByIdx[i], role: i % 2 === 1 ? 'buyer' : 'seller' });
    });
  }
  return { users };
}

function authHeaders(token) {
  return { 'Content-Type': 'application/json', Authorization: `Bearer ${token}` };
}

// 주문 의도마다 새 키. 형식과 충돌 없음은 idempotency-key.js가 정의하고 셀프체크가
// 검증한다. 재사용하면 두 번째 주문부터 전부 replay가 되어 체결 부하가 사라진다.
const GEN_ID = __ENV.GEN_ID || 'gen';
const RUN_ID = __ENV.RUN_ID || String(Date.now());
let orderSeq = 0;
function newIdempotencyKey() {
  orderSeq += 1;
  return buildIdempotencyKey(GEN_ID, RUN_ID, __VU, __ITER, orderSeq);
}

export function runVU(data) {
  const user = data.users[Math.floor(Math.random() * data.users.length)];
  // 항상 상대편을 넘는 가격(±50틱) — 대기 중인 반대편 주문이 있으면 즉시 체결.
  const price = user.role === 'buyer' ? BASE_PRICE + 50 * TICK : BASE_PRICE - 50 * TICK;
  http.post(
    `${BASE_URL}/orders`,
    JSON.stringify({
      coin_symbol: COIN_SYMBOL,
      side: user.role === 'buyer' ? 'BUY' : 'SELL',
      order_type: 'LIMIT',
      price: String(price),
      amount: ORDER_AMOUNT,
    }),
    {
      headers: { ...authHeaders(user.token), 'Idempotency-Key': newIdempotencyKey() },
      tags: { name: 'create_order' },
      responseCallback: orderResponseCallback,
      timeout: '30s',
    }
  );
  if (SLEEP_MS > 0) sleep(SLEEP_MS);
}
