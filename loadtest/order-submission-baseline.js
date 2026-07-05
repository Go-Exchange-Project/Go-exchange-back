import http from 'k6/http';
import { check, sleep } from 'k6';
import exec from 'k6/execution';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const DEV_TOOLS_TOKEN = __ENV.DEV_TOOLS_TOKEN;
const DEV_TOOLS_TOKEN_HEADER = 'X-GoExchange-Dev-Token';

const TOTAL_USERS = 50;
const COIN_SYMBOL = 'BTC';
const FIXED_PRICE = '50000000';
const ORDER_AMOUNT = '0.001';
const BUYER_KRW_FUNDING = '100000000';
const SELLER_BTC_FUNDING = '1000';

const FULL_STAGES = [
  { duration: '30s', target: 10 },
  { duration: '1m', target: 50 },
  { duration: '1m', target: 50 },
  { duration: '20s', target: 0 },
];

const SMOKE_STAGES = [
  { duration: '5s', target: 2 },
  { duration: '10s', target: 2 },
  { duration: '5s', target: 0 },
];

export const options = {
  scenarios: {
    order_submission_baseline: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: __ENV.SMOKE_TEST === 'true' ? SMOKE_STAGES : FULL_STAGES,
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
    // Alternate role by parity (not a contiguous 1..25/26..50 split) so that
    // any two consecutive VU IDs include one buyer and one seller. This
    // matters because k6 assigns VU IDs sequentially starting at 1, and a
    // low-VU smoke test (e.g. target=2) would otherwise only ever activate
    // VU1/VU2 — which must include both roles to produce a trade.
    const role = i % 2 === 1 ? 'buyer' : 'seller';
    const email = `loadtest-user-${i}@test.local`;

    // Register-or-login: re-running this script against the same test DB
    // (e.g. smoke test then full test, or two full runs back to back) hits
    // "409 email already registered" for users created by a prior run. Fall
    // back to logging in as that user instead of treating it as a failure,
    // so the script is safely re-runnable without resetting the test DB.
    const registerRes = http.post(
      `${BASE_URL}/auth/register`,
      JSON.stringify({
        name: `Load Test User ${i}`,
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
