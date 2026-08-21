import http from 'k6/http';
import { check, sleep } from 'k6';
import exec from 'k6/execution';
import { Counter, Rate } from 'k6/metrics';
import { classifyOrderResponse, classifyCancelResponse } from './sli-classify.js';
import { buildLevelPlan, levelForElapsed } from './level-classify.js';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const DEV_TOOLS_TOKEN = __ENV.DEV_TOOLS_TOKEN;
const DEV_TOOLS_TOKEN_HEADER = 'X-GoExchange-Dev-Token';

// 23번(⑤): 22번 order-spike-single-symbol.js 계승. 목표 5,000 hold + 초과
// ~10,000 버스트를 담기 위해 TOTAL_USERS를 피크 VU(10,000)에 맞춘다(1 VU = 1 user,
// 지갑 공유로 인한 잔고 경합 혼선을 피한다 — 22번과 동일 원칙).
const TOTAL_USERS = parseInt(__ENV.TOTAL_USERS || '10000', 10);
// 26번(23번 재실행): load-gen 수평 2대 분할 시 유저 인덱스가 겹치지 않게 오프셋을
// 둔다 — VM A는 OFFSET=0, VM B는 OFFSET=5000. 각 VM은 자기 범위만 setup한다.
const USER_INDEX_OFFSET = parseInt(__ENV.USER_INDEX_OFFSET || '0', 10);
// 26번: 두 VM의 setup(등록·로그인·펀딩) 소요 시간이 달라도 시나리오가 같은 UTC
// 시각에 시작하도록 하는 배리어. 0이면 비활성(기존 단일 VM 동작과 동일).
const LOAD_START_AT_MS = parseInt(__ENV.LOAD_START_AT_MS || '0', 10);
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
const cancelResponseCallback = http.expectedStatuses(200, 202, 404, 409);
// 주문 생성의 503(입장 거절)은 ④의 의도된 "우아한 셰딩" — http_req_failed로 안 셈,
// custom_fast_reject_503로 별도 집계.
const orderResponseCallback = http.expectedStatuses(200, 201, 503);

// 3차③: 뭉뚱그린 가용성을 세 독립 SLI로 분리(정의는 sli-classify.js). threshold는
// 안 건다 — 이 단계는 정의+기준선 수집.
const RESPONSE_SLO_MS = parseInt(__ENV.RESPONSE_SLO_MS || '1000', 10);

const orderResponseAvailability = new Rate('sli_order_response_availability');
const orderBusinessSuccess = new Rate('sli_order_business_success');
const cancelSuccessSli = new Rate('sli_cancel_success');

// 32번: 1초 계약 초과 "건수"를 판정값으로 쓰므로 전용 Counter가 필요하다.
// sli_order_response_availability의 fail에는 느린 응답뿐 아니라 status 0·예상 밖
// 상태도 섞이므로 그 지표로는 duration > SLO 건수를 셀 수 없다.
const orderResponseOverSlo = new Counter('sli_order_response_over_slo_total');

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

// 32번(용량 경계 탐색) 전용 프로파일. PROFILE=capacity 일 때만 활성화되며,
// 기존 스파이크 프로파일은 그대로 둔다 — 26~31번 재현성을 깨지 않기 위해서다.
//
// CAPACITY_LEVELS는 **load-gen 1대 기준 VU**다(기존 STAGEn_VUS와 같은 단위).
// vu_level 라벨과 판정표는 **합산 VU**이므로 VU_LEVEL_SCALE(load-gen 대수)을 곱한다.
// 기본값 250/500/1,000/2,000(대당) = 합산 500/1,000/2,000/4,000.
const PROFILE = __ENV.PROFILE || 'spike';
const VU_LEVEL_SCALE = parseInt(__ENV.VU_LEVEL_SCALE || '2', 10);
const CAPACITY_LEVELS = (__ENV.CAPACITY_LEVELS || '250,500,1000,2000')
  .split(',')
  .map((value) => parseInt(value.trim(), 10))
  .filter((value) => value > 0);
const CAPACITY_RAMP = __ENV.CAPACITY_RAMP || '30s';
const CAPACITY_HOLD = __ENV.CAPACITY_HOLD || '3m';

// 계단마다 (ramp, hold) 두 stage — 6개 고정인 SPIKE_STAGES로는 표현할 수 없다.
const CAPACITY_STAGES = [];
for (const level of CAPACITY_LEVELS) {
  CAPACITY_STAGES.push({ duration: CAPACITY_RAMP, target: level });
  CAPACITY_STAGES.push({ duration: CAPACITY_HOLD, target: level });
}

const IS_CAPACITY = PROFILE === 'capacity';
const ACTIVE_STAGES = IS_CAPACITY ? CAPACITY_STAGES : SPIKE_STAGES;
const LEVEL_PLAN = IS_CAPACITY ? buildLevelPlan(CAPACITY_STAGES, 0, VU_LEVEL_SCALE) : null;

// 태그를 달아도 threshold 선언이 없으면 서브메트릭이 summary에 나타나지 않는다.
// 판정은 사람이 하므로 항상 통과하는 형태로 둔다(런을 중단시키지 않는다).
function capacityThresholds() {
  const thresholds = {};
  for (const level of CAPACITY_LEVELS) {
    const label = String(level * VU_LEVEL_SCALE);
    thresholds[`sli_order_response_availability{vu_level:${label}}`] = ['rate>=0'];
    thresholds[`sli_order_business_success{vu_level:${label}}`] = ['rate>=0'];
    thresholds[`sli_cancel_success{vu_level:${label}}`] = ['rate>=0'];
    thresholds[`sli_order_response_over_slo_total{vu_level:${label}}`] = ['count>=0'];
  }
  return thresholds;
}

export const options = {
  setupTimeout: '25m',
  batch: SETUP_BATCH_SIZE,
  batchPerHost: SETUP_BATCH_SIZE,
  scenarios: {
    order_spike_availability: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: ACTIVE_STAGES,
      exec: 'runVU',
    },
  },
  // 판정 실패로 런을 중단시키지 않는다 — 프로파일 전체를 완주시켜 전부 기록.
  thresholds: IS_CAPACITY ? capacityThresholds() : {},
};

// 시나리오 시작 시각(ms). 배리어가 setup을 붙잡았다 놓으므로 두 VM에서 거의 같다.
// exec.scenario.startTime을 우선 쓰고, 없으면 배리어 값으로 대체한다.
function scenarioStartMs() {
  const started = exec.scenario.startTime;
  if (typeof started === 'number' && started > 0) return started;
  return LOAD_START_AT_MS > 0 ? LOAD_START_AT_MS : 0;
}

// 표본이 속한 계단 라벨. capacity 프로파일이 아니거나 기준 시각이 없으면 태깅하지 않는다.
// 반드시 **요청 시작 시각**을 넘겨야 한다 — 응답 완료 시각으로 계산하면 hold 종료
// 직전에 시작해 다음 ramp에서 끝난 요청이 잘못된 계단에 들어간다.
function levelTags(startedAtMs) {
  if (!LEVEL_PLAN) return undefined;
  const base = scenarioStartMs();
  if (!base) return undefined;
  return { vu_level: levelForElapsed(startedAtMs - base, LEVEL_PLAN) };
}

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
        name: `Spike Test User ${USER_INDEX_OFFSET + i}`,
        email: `spike-user-${USER_INDEX_OFFSET + i}@test.local`,
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
        throw new Error(`setup: failed to register user ${USER_INDEX_OFFSET + i}: ${res.status} ${res.body}`);
      }
    });

    if (loginNeeded.length > 0) {
      const loginRequests = loginNeeded.map((i) => [
        'POST',
        `${BASE_URL}/auth/login`,
        JSON.stringify({ email: `spike-user-${USER_INDEX_OFFSET + i}@test.local`, password: 'loadtest-password-123' }),
        { headers: { 'Content-Type': 'application/json' }, tags: { name: 'setup' } },
      ]);
      const loginResponses = http.batch(loginRequests);
      loginResponses.forEach((res, idx) => {
        const i = loginNeeded[idx];
        if (res.status !== 200) {
          throw new Error(`setup: user ${USER_INDEX_OFFSET + i} already registered but login failed: ${res.status} ${res.body}`);
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
        throw new Error(`setup: failed to fund wallet for user ${USER_INDEX_OFFSET + i}: ${res.status} ${res.body}`);
      }
      const role = i % 2 === 1 ? 'buyer' : 'seller';
      users.push({ token: tokensByIndex[i], role });
    });
  }

  // 26번: setup(등록·로그인·펀딩)을 끝낸 뒤에만 공통 시작 시각까지 대기한다 —
  // 두 VM의 setup 소요 시간 차이가 그대로 ramp 시작 skew가 되는 것을 막는다.
  // 시각을 이미 넘겼으면 이 런은 폐기(throw)한다 — 억지로 진행하지 않는다.
  const remainingMs = LOAD_START_AT_MS - Date.now();
  if (LOAD_START_AT_MS > 0 && remainingMs <= 0) {
    throw new Error('setup missed the coordinated load start deadline');
  }
  if (remainingMs > 0) {
    sleep(remainingMs / 1000);
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
  // 계단 라벨은 **요청 시작 시각** 기준이다(level-classify.js 주석 참조).
  const startedAt = Date.now();
  const tags = levelTags(startedAt);
  const res = http.post(`${BASE_URL}/orders`, JSON.stringify(body), {
    headers: authHeaders(user.token),
    tags: { name: 'create_order' },
    responseCallback: orderResponseCallback,
    timeout: '90s',
  });
  // Retry-After sleep 전에 분류·add — sleep은 클라 백오프이지 서버 지연이
  // 아니므로 응답 가용성 시간에 섞이면 안 된다.
  const cls = classifyOrderResponse(res.status, res.timings.duration, RESPONSE_SLO_MS);
  orderResponseAvailability.add(cls.available, tags);
  orderBusinessSuccess.add(cls.businessSuccess, tags);
  if (res.timings.duration > RESPONSE_SLO_MS) {
    orderResponseOverSlo.add(1, tags);
  }
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

  // 취소도 요청 시작 시각 기준으로 태깅한다 — 위 sleep(1~3초) 때문에 주문 시점의
  // 계단과 다를 수 있고, 그 경우 취소가 실제로 발생한 계단에 귀속돼야 한다.
  const cancelStartedAt = Date.now();
  const cancelTags = levelTags(cancelStartedAt);
  const cancelRes = http.del(`${BASE_URL}/orders/${orderId}`, null, {
    headers: authHeaders(user.token),
    tags: { name: 'cancel_order' },
    responseCallback: cancelResponseCallback,
  });
  if (cancelRes.status === 200 || cancelRes.status === 202) {
    cancelSuccess.add(1);
  } else if (cancelRes.status === 404 || cancelRes.status === 409) {
    cancelAlreadyFilled.add(1);
  } else {
    cancelFail.add(1);
  }

  // 404/409는 정상 경쟁 결과라 분모에서 자연 제외(add 호출 안 함).
  const cancelClass = classifyCancelResponse(cancelRes.status);
  if (cancelClass === 'success') cancelSuccessSli.add(true, cancelTags);
  else if (cancelClass === 'infra_fail') cancelSuccessSli.add(false, cancelTags);
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
