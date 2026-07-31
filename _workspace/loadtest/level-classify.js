// 32번(용량 경계 탐색): 계단식 프로파일에서 각 표본이 속한 hold 계단을 판정한다.
//
// 순수 함수로 분리한 이유: 경계 처리(hold 종료 직전·정확히·직후)가 틀리면 표본이
// 잘못된 계단에 들어가 판정 자체가 오염된다. 단위 테스트로 고정해야 하는 로직이다.
//
// 판정은 **요청 시작 시각** 기준으로 해야 한다(호출부 책임) — 응답 완료 시각으로 하면
// hold 종료 직전에 시작해 다음 ramp에서 끝난 요청이 잘못된 계단에 들어간다.

const DURATION_UNITS_MS = { ms: 1, s: 1000, m: 60000, h: 3600000 };

// k6 duration 문자열('30s', '3m', '1m30s')을 밀리초로 바꾼다.
export function parseDurationMs(text) {
  const matches = String(text).match(/(\d+(?:\.\d+)?)(ms|s|m|h)/g);
  if (!matches || matches.length === 0) {
    throw new Error(`level-classify: unsupported duration "${text}"`);
  }
  let total = 0;
  for (const part of matches) {
    const [, value, unit] = part.match(/(\d+(?:\.\d+)?)(ms|s|m|h)/);
    total += parseFloat(value) * DURATION_UNITS_MS[unit];
  }
  return total;
}

// stages를 [{startMs, endMs, label}] 구간 목록으로 편다.
// - target이 직전 target과 같으면 hold, 다르면 ramp다.
// - hold의 label은 `target * scale`(scale = load-gen 대수) — 판정표가 합산 VU 기준이다.
// - 구간은 [startMs, endMs) 반열린 구간이다. 정확히 endMs인 표본은 다음 구간에 속한다.
export function buildLevelPlan(stages, startTarget, scale) {
  const factor = scale > 0 ? scale : 1;
  const plan = [];
  let cursor = 0;
  let previous = startTarget;
  for (const stage of stages) {
    const durationMs = parseDurationMs(stage.duration);
    const isHold = stage.target === previous;
    plan.push({
      startMs: cursor,
      endMs: cursor + durationMs,
      label: isHold ? String(stage.target * factor) : 'ramp',
    });
    cursor += durationMs;
    previous = stage.target;
  }
  return plan;
}

// 경과 시간이 속한 구간의 label을 돌려준다.
// 시나리오 시작 전('pre')과 마지막 구간 이후('post')는 분석에서 제외한다.
export function levelForElapsed(elapsedMs, plan) {
  if (elapsedMs < 0) return 'pre';
  for (const segment of plan) {
    if (elapsedMs >= segment.startMs && elapsedMs < segment.endMs) return segment.label;
  }
  return 'post';
}
