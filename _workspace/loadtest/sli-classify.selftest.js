import { classifyOrderResponse, classifyCancelResponse } from './sli-classify.js';

export const options = { vus: 1, iterations: 1 };

function assert(cond, msg) {
  if (!cond) throw new Error('SLI selftest FAILED: ' + msg);
}

export default function () {
  const SLO = 1000;
  // 주문 판정표
  let r = classifyOrderResponse(201, 500, SLO);
  assert(r.available && r.businessSuccess, '201 ≤SLO → 가용·업무 성공');
  r = classifyOrderResponse(201, 24000, SLO);
  assert(!r.available && r.businessSuccess, '201 >SLO → 가용 실패·업무 성공(느린 2xx)');
  r = classifyOrderResponse(200, 130, SLO);
  assert(r.available && r.businessSuccess, '200 ≤SLO → 둘 다 성공');
  r = classifyOrderResponse(503, 0.2, SLO);
  assert(r.available && !r.businessSuccess, '503 ≤SLO → 가용 성공·업무 실패');
  r = classifyOrderResponse(503, 1500, SLO);
  assert(!r.available && !r.businessSuccess, '503 >SLO → 둘 다 실패');
  r = classifyOrderResponse(0, 90000, SLO);
  assert(!r.available && !r.businessSuccess, 'status 0 → 둘 다 실패');
  // 취소 판정표
  assert(classifyCancelResponse(200) === 'success', '취소 200 → success');
  assert(classifyCancelResponse(404) === 'excluded', '취소 404 → excluded');
  assert(classifyCancelResponse(409) === 'excluded', '취소 409 → excluded');
  assert(classifyCancelResponse(0) === 'infra_fail', '취소 status 0 → infra_fail');
  assert(classifyCancelResponse(500) === 'infra_fail', '취소 500 → infra_fail');
  assert(classifyCancelResponse(502) === 'infra_fail', '취소 5xx → infra_fail');
  console.log('SLI selftest PASSED');
}
