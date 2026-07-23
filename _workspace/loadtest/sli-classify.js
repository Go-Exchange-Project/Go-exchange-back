// 응답 상태·지연을 두 SLI 판정으로 분류(순수 — k6 의존 없음, 셀프체크가 검증).
export function classifyOrderResponse(status, durationMs, sloMs) {
  const contracted = status === 200 || status === 201 || status === 503;
  return {
    available: contracted && durationMs <= sloMs, // 느린 2xx/503도 가용 실패
    businessSuccess: status === 200 || status === 201, // 503은 업무 실패(지연 무관)
  };
}

// 취소 성공률 분류: 404/409는 정상 경쟁이라 분모 제외, 그 외 비-200(status 0·5xx 포함)은 인프라 실패.
export function classifyCancelResponse(status) {
  if (status === 200) return 'success';
  if (status === 404 || status === 409) return 'excluded';
  return 'infra_fail';
}
