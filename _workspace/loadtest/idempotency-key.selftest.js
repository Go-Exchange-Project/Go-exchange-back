import { buildIdempotencyKey } from './idempotency-key.js';

export const options = { vus: 1, iterations: 1 };

function assert(cond, msg) {
  if (!cond) throw new Error('idempotency key selftest FAILED: ' + msg);
}

export default function () {
  // 같은 입력이면 같은 키다 — 재시도가 같은 키를 다시 만들 수 있어야 한다.
  assert(
    buildIdempotencyKey('genA', 'run1', 3, 7, 2) === buildIdempotencyKey('genA', 'run1', 3, 7, 2),
    '같은 입력이 다른 키를 만들었다 — 재시도가 중복 주문이 된다',
  );

  // 한 축이라도 다르면 키가 달라야 한다.
  const base = buildIdempotencyKey('genA', 'run1', 3, 7, 2);
  assert(buildIdempotencyKey('genB', 'run1', 3, 7, 2) !== base, 'load-gen이 다른데 키가 같다');
  assert(buildIdempotencyKey('genA', 'run2', 3, 7, 2) !== base, 'run이 다른데 키가 같다');
  assert(buildIdempotencyKey('genA', 'run1', 4, 7, 2) !== base, 'VU가 다른데 키가 같다');
  assert(buildIdempotencyKey('genA', 'run1', 3, 8, 2) !== base, 'iteration이 다른데 키가 같다');
  assert(buildIdempotencyKey('genA', 'run1', 3, 7, 3) !== base, '주문 순번이 다른데 키가 같다');

  // 실제 조합 공간에서 충돌이 없는지 본다. 두 load-gen × VU × iteration × 주문 순번.
  const seen = {};
  let total = 0;
  for (const gen of ['gen0', 'gen5000']) {
    for (let vu = 1; vu <= 40; vu++) {
      for (let iter = 0; iter < 20; iter++) {
        for (let seq = 1; seq <= 3; seq++) {
          const key = buildIdempotencyKey(gen, 'run1', vu, iter, seq);
          assert(!seen[key], `키가 충돌했다: ${key}`);
          seen[key] = true;
          total += 1;
        }
      }
    }
  }
  assert(total === 2 * 40 * 20 * 3, `조합 수가 맞지 않는다: ${total}`);

  // 축을 구분자 없이 이어 붙이면 (vu=1, iter=23)과 (vu=12, iter=3)이 충돌한다.
  // 구분자가 실제로 그것을 막는지 확인한다.
  assert(
    buildIdempotencyKey('g', 'r', 1, 23, 1) !== buildIdempotencyKey('g', 'r', 12, 3, 1),
    '축 경계가 없어 서로 다른 (VU, iteration)이 같은 키가 됐다',
  );

  console.log('idempotency key selftest PASSED: ' + total + ' keys, no collision');
}
