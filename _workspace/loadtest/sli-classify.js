// 응답 상태·지연을 SLI 판정으로 분류(순수 — k6 의존 없음, 셀프체크가 검증).
//
// 202는 멱등성 계약의 PENDING이다. 주문 행과 hold는 이미 커밋됐고 서버가 그 뒤를
// durable하게 확정하지 못했을 뿐이므로 거절이 아니다 — 업무 실패로 세면 정상 주문이
// 실패로 잡힌다. 다만 성공에 그냥 섞으면 "outcome UPDATE가 실패하고 있다"는 신호가
// 사라지므로 pendingOutcome으로 따로 드러낸다.
//
// 400(키 누락)·409(같은 키 다른 요청)는 계약 위반이다. contracted에 넣지 않아
// 가용·업무 양쪽 실패로 남긴다 — 하니스의 키 생성이 깨졌다는 뜻이므로 숨기면 안 된다.
export function classifyOrderResponse(status, durationMs, sloMs) {
  const contracted = status === 200 || status === 201 || status === 202 || status === 503;
  return {
    available: contracted && durationMs <= sloMs, // 느린 2xx/503도 가용 실패
    businessSuccess: status === 200 || status === 201 || status === 202, // 503은 업무 실패(지연 무관)
    pendingOutcome: status === 202,
  };
}

// 취소 성공률 분류: 404/409는 정상 경쟁이라 분모 제외, 그 외(status 0·5xx 포함)는 인프라 실패.
//
// 202는 "취소 의도가 내구적으로 저장됐다"는 현재 계약이고, 200은 그 전 계약이다.
// 둘 다 성공으로 둔다 — 202를 빠뜨리면 취소 성공률이 0%로 보인다.
export function classifyCancelResponse(status) {
  if (status === 200 || status === 202) return 'success';
  if (status === 404 || status === 409) return 'excluded';
  return 'infra_fail';
}
