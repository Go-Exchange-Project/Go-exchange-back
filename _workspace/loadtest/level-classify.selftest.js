import { parseDurationMs, buildLevelPlan, levelForElapsed } from './level-classify.js';

export const options = { vus: 1, iterations: 1 };

function assert(cond, msg) {
  if (!cond) throw new Error('level selftest FAILED: ' + msg);
}

export default function () {
  // duration 파싱
  assert(parseDurationMs('30s') === 30000, "'30s' → 30000ms");
  assert(parseDurationMs('3m') === 180000, "'3m' → 180000ms");
  assert(parseDurationMs('1m30s') === 90000, "'1m30s' → 90000ms");
  assert(parseDurationMs('500ms') === 500, "'500ms' → 500ms");

  // 계단 2개(합산 500 → 1,000), load-gen 2대 → scale=2, 스크립트 target은 대당 값
  const stages = [
    { duration: '30s', target: 250 }, // ramp 0→250
    { duration: '3m', target: 250 }, // hold 250 (합산 500)
    { duration: '30s', target: 500 }, // ramp 250→500
    { duration: '3m', target: 500 }, // hold 500 (합산 1,000)
  ];
  const plan = buildLevelPlan(stages, 0, 2);

  assert(plan.length === 4, '구간 4개');
  assert(plan[0].label === 'ramp', '첫 구간은 ramp');
  assert(plan[1].label === '500', 'hold 250×2 → 라벨 500');
  assert(plan[2].label === 'ramp', '계단 사이는 ramp');
  assert(plan[3].label === '1000', 'hold 500×2 → 라벨 1000');

  // 경계 3지점 — hold 종료 직전 / 정확히 종료 / 직후
  const holdEnd = plan[1].endMs; // 30s + 3m = 210,000ms
  assert(holdEnd === 210000, 'hold 종료가 210,000ms');
  assert(levelForElapsed(holdEnd - 1, plan) === '500', '종료 −1ms → 여전히 500 계단');
  assert(levelForElapsed(holdEnd, plan) === 'ramp', '정확히 종료 시각 → 다음 구간(ramp)');
  assert(levelForElapsed(holdEnd + 1, plan) === 'ramp', '종료 +1ms → ramp');

  // hold 시작 경계도 같은 규칙
  const holdStart = plan[1].startMs; // 30,000ms
  assert(levelForElapsed(holdStart - 1, plan) === 'ramp', '시작 −1ms → 아직 ramp');
  assert(levelForElapsed(holdStart, plan) === '500', '정확히 시작 시각 → 500 계단');

  // 범위 밖
  assert(levelForElapsed(-1, plan) === 'pre', '음수 경과 → pre');
  assert(levelForElapsed(plan[3].endMs, plan) === 'post', '마지막 구간 종료 → post');
  assert(levelForElapsed(plan[3].endMs - 1, plan) === '1000', '마지막 구간 종료 −1ms → 1000');

  // scale=1(단일 load-gen)이면 라벨이 대당 값 그대로
  const solo = buildLevelPlan(stages, 0, 1);
  assert(solo[1].label === '250', 'scale=1 → 라벨 250');

  console.log('level selftest PASSED');
}
