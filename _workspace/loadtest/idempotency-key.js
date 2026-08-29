// 주문 생성 멱등성 키의 형식(순수 — k6 의존 없음, 셀프체크가 검증).
//
// 키가 유일해야 하는 이유와 유일하면 안 되는 지점이 서로 반대다.
//   - VU 전체에서 재사용하면 두 번째 주문부터 전부 replay가 되어 주문이 생성되지
//     않는다. 부하가 사라지므로 측정이 무의미해진다.
//   - 반대로 같은 주문의 재시도마다 새 키를 만들면 중복 주문이 생기고, 멱등성을
//     전혀 검증하지 못한다. 재시도는 이미 만든 키를 그대로 다시 써야 한다.
//
// generator를 넣는 이유: load-gen 2대가 같은 __VU·__ITER를 동시에 돌린다. run을
// 넣는 이유: 같은 VM에서 두 번 돌리면 __VU·__ITER가 그대로 반복된다.
export function buildIdempotencyKey(genID, runID, vu, iter, seq) {
  return `${genID}-${runID}-${vu}-${iter}-${seq}`;
}
