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

  // 멱등성 계약: 202(PENDING)는 주문·hold가 이미 커밋된 상태다. 실패로 세면 정상
  // 주문이 실패로 잡히고, 그냥 성공에 섞으면 outcome UPDATE 실패 신호가 사라진다.
  r = classifyOrderResponse(202, 120, SLO);
  assert(r.available && r.businessSuccess, '202 ≤SLO → 가용·업무 성공');
  assert(r.pendingOutcome, '202는 pendingOutcome으로 따로 드러나야 한다');
  r = classifyOrderResponse(202, 24000, SLO);
  assert(!r.available && r.businessSuccess, '202 >SLO → 가용 실패·업무 성공(느린 2xx)');
  assert(!classifyOrderResponse(200, 120, SLO).pendingOutcome, '200은 pendingOutcome이 아니다');

  // 오류 counter가 실제 실패를 숨기지 않는지 고정한다. 400(키 누락)·409(같은 키
  // 다른 요청)는 하니스의 키 생성이 깨졌다는 뜻이므로 반드시 실패로 남아야 한다.
  r = classifyOrderResponse(400, 5, SLO);
  assert(!r.available && !r.businessSuccess, '400(키 누락) → 둘 다 실패');
  r = classifyOrderResponse(409, 5, SLO);
  assert(!r.available && !r.businessSuccess, '409(같은 키 다른 요청) → 둘 다 실패');
  r = classifyOrderResponse(500, 5, SLO);
  assert(!r.available && !r.businessSuccess, '500 → 둘 다 실패');
  r = classifyOrderResponse(502, 5, SLO);
  assert(!r.available && !r.businessSuccess, '502 → 둘 다 실패');
  // 취소 판정표
  // 200은 202 전환 전 계약이다. 두 값을 모두 성공으로 두어 구·신 하니스가
  // 같은 분류기를 쓸 수 있게 한다.
  assert(classifyCancelResponse(200) === 'success', '취소 legacy 200 → success');
  assert(classifyCancelResponse(202) === 'success', '취소 202 → success');
  assert(classifyCancelResponse(404) === 'excluded', '취소 404 → excluded');
  assert(classifyCancelResponse(409) === 'excluded', '취소 409 → excluded');
  assert(classifyCancelResponse(0) === 'infra_fail', '취소 status 0 → infra_fail');
  assert(classifyCancelResponse(500) === 'infra_fail', '취소 500 → infra_fail');
  assert(classifyCancelResponse(502) === 'infra_fail', '취소 5xx → infra_fail');
  console.log('SLI selftest PASSED');
}
