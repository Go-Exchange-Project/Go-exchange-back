# 19번째 테스트 (2026-07-16): B-1a/B-1b/B-4 일괄 GCP 재측정

## 커밋 해시

- **A (before)**: `bc8c00f` — 18번 벤치마크의 opt(MarkProcessed 흡수), B-1/B-4 이전 마지막 측정 코드
- **B (after)**: `f3103c0` — 현 main. B-1a(스냅샷 코얼레싱+캐시, `31f0f80`) + B-1b(WS 심볼 구독, `51ef2e1`) + B-4(정산 그룹커밋, 6커밋) 누적

## 왜 이 테스트를 했는지

[5번](../refactor/5_B-1_스냅샷_코얼레싱과_WS_구독_완료.md)·[6번](../refactor/6_B-4_정산_그룹커밋_완료.md) 완료 문서 모두 "GCP 측정은 다음 사이클에 일괄"을 유일한 잔여 항목으로 남겼다. 세 변경(B-1a 코얼레싱, B-1b WS 구독, B-4 그룹커밋)이 누적됐을 때의 실제 처리량 효과와, WS 부하 하에서 매칭 엔진 결박이 실제로 해소됐는지를 같은 세션 A/B로 검증한다.

## 왜 이 방식을 선택했는지 (방법론 — 17·18번 계승)

- **같은 VM·같은 세션 연속 A/B**: 서버 VM은 이번 세션 동안 한 번도 stop/start하지 않았다(18번에서 실증된 +24% 세션 교란을 피하기 위함). A(`bc8c00f`)로 측정 1·2 → B(`f3103c0`)로 재배포 후 측정 3·4·5, 전부 한 세션.
- **매 측정 전 리셋**: TRUNCATE(9개 데이터 테이블, `goose_db_version` 제외, `RESTART IDENTITY CASCADE`) + backend 재시작 + `matching bootstrap completed: loaded=0` 로그 확인. 5회 전부 수행.
- **hold 프로파일**: 1.5분 워밍업(VU 0→3000) → 3000에서 8분 유지. `~/stress-hold3000.js`가 이전 세션 이후 load-gen VM에서 유실되어(재기동 시 디스크가 초기화된 것으로 추정), `loadtest/order-submission-stress.js`(램프업용, 저장소에 커밋됨) 구조를 그대로 가져와 `TOTAL_USERS=3000`·hold 스테이지로 재작성했다. 원문은 문서 하단에 첨부.
- **WS 부하는 별도 k6 프로세스**로 REST hold와 동시 실행(같은 load-gen VM, `k6/ws` 클래식 API). 서버 `ServeWs`의 Origin 체크를 피하기 위해 서버 env는 건드리지 않고 k6 쪽에서 기본 허용 목록에 있는 `Origin: http://localhost:3000`을 접속 헤더에 실었다(`internal/ws/handler.go`의 `defaultWSAllowedOrigins` 확인).
- **DEV_TOOLS_TOKEN 전달**: 서버 VM `.env`의 토큰을 트랜스크립트에 노출하거나 영구 저장하지 않기 위해, 서버→로컬 스크래치패드(사용자 승인 하 임시 경유)→load-gen VM 경로로 전달 후 로컬 사본은 즉시 삭제. load-gen VM에는 `~/.devtoolstoken`(권한 600)으로만 존재.

### 예상 밖 이슈: load-gen VM 리사이즈 (측정 3 진행 중)

측정 3(B, REST 단독) 최초 시도가 진행 277초 시점에 k6 프로세스가 OOM으로 강제 종료됐다(`dmesg`: `Out of memory: Killed process 3043 (k6) ... anon-rss:7780520kB`). 원인 조사:

- 서버 측 로그(`docker logs goexchange-stress-backend`)를 보면 09:16:40 이후 `/orders` 요청이 완전히 끊기고 헬스체크(`/ping`)만 남았다 — **서버는 정상**이었고, 문제는 k6 자신이었다.
- B가 요청당 8~18ms로 응답할 만큼 빨라지자(A는 초 단위), VU 3000개가 응답 대기 없이 거의 연속으로 루프를 돌면서 k6 자체의 요청/체크/메트릭 처리량이 e2-standard-2(2 vCPU, 7.8GB)의 용량을 넘었다. 서버가 아니라 **부하생성기가 병목이 된 것**.
- 사용자 승인 하에 **load-gen VM만** `e2-standard-2` → `e2-standard-8`(8 vCPU, 31GB)로 stop→resize→start. **서버·DB VM은 전혀 건드리지 않아** "같은 세션" 규칙(물리 호스트 고정)은 그대로 유지된다 — 이 규칙은 측정 대상인 서버 VM에 대한 것이지 부하생성기에는 적용되지 않는다고 판단했고, 사용자가 그 해석에 동의했다.
- 리사이즈 후 DB 재리셋 + backend 재시작으로 측정 3을 처음부터 재실행했다. 이하 수치는 전부 재실행분이다.

## 실행한 정확한 커맨드

리셋(db VM, 컨테이너 내부):

```bash
docker exec goexchange-db-postgres bash -c \
  'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "TRUNCATE TABLE failed_market_completions, failed_settlements, ledger_entries, orders, reconciliation_violations, trade_outbox_events, trades, users, wallets RESTART IDENTITY CASCADE;"'
```

배포(서버 VM, 커밋별 소스 디렉터리에서):

```bash
docker compose -f docker-compose.stress.yml up -d --build backend   # A: bench-opt(bc8c00f), B: bench-b19(f3103c0)
docker compose -f docker-compose.stress.yml restart backend          # 리셋마다
```

REST hold(load-gen VM):

```bash
k6 run -e BASE_URL=http://10.10.0.3:8080 -e DEV_TOOLS_TOKEN=$(cat ~/.devtoolstoken) ~/stress-hold3000.js
```

WS 부하(REST와 동시, 별도 프로세스):

```bash
# legacy(측정 2, 4)
k6 run -e BASE_URL=http://10.10.0.3:8080 -e MODE=legacy -e N=300 -e DURATION=9m40s -e HOLD_MS=570000 ~/ws-load.js
# subscribe(측정 5)
k6 run -e BASE_URL=http://10.10.0.3:8080 -e MODE=subscribe -e N=300 -e DURATION=9m40s -e HOLD_MS=570000 ~/ws-load.js
```

환경: 서버 `e2-highcpu-4`(4 vCPU), DB `e2-highcpu-4`(별도 VM), load-gen `e2-standard-8`(측정 3 재시도부터, 최초는 `e2-standard-2`), k6 v2.0.0, 서울 리전. 이전 세션(17·18번) 문서의 "서버 e2-medium" 기록과 다른데, 그 사이 [11번 벤치마크(VM CPU 스케일링 제어)](11-2026-07-09-vm-cpu-scaling-control.md)의 절차로 서버 스펙이 바뀐 것으로 보인다 — 이번 세션 내 A/B는 동일 스펙에서 연속 측정했으므로 세션 내부 비교는 유효하다.

## 핵심 결과

### REST 처리량 (측정 1·2·3·4·5)

| # | 빌드 | WS | iterations | 처리량(iter/s) | http_req p95 | http_req_failed |
|---|---|---|---|---|---|---|
| 1 | A | 없음 | 103,103 | **156.46** | 18.19s | 0.00% |
| 2 | A | legacy×300 | 78,578 | **119.10** (−23.9% vs 1) | 24.81s | 0.00% |
| 3 | B | 없음 | 554,469 | **861.06** (**+450.3%** vs 1) | 2.94s | 0.00% |
| 4 | B | legacy×300 | 368,494 | **572.44** (−33.5% vs 3) | 4.72s | 0.00% |
| 5 | B | subscribe×300(BTC) | 362,961 | **563.98** (−34.5% vs 3) | 4.75s | 0.00% |

### settlement_batch_size 스냅샷 (측정 3·4·5 직후 `/metrics`)

| # | count | avg size | ≤16 비중 | ≤32(=상한) 비중 | fallbacks |
|---|---|---|---|---|---|
| 3 | 8,909 | 31.11 | 3.1% | 100% | 0 |
| 4 | 5,923 | 31.10 | 3.2% | 100% | 0 |
| 5 | 5,826 | 31.15 | 3.0% | 100% | 0 |

세 측정 모두 배치의 97% 이상이 상한(32건)에 몰려 있다 — B-4 그룹커밋이 이 부하에서 **거의 항상 최대 크기로 가득 찬 배치**를 커밋한다는 뜻이고, `fallbacks_total`이 0이므로 배치 실패로 인한 단건 폴백도 없었다.

### WS 클라이언트 수신 메시지 (측정 2·4·5, 300 클라이언트·9.5분)

| # | 빌드 | 모드 | 총 수신 메시지 | 클라이언트당 평균 |
|---|---|---|---|---|
| 2 | A | legacy | 29,866,208 | 99,554 |
| 4 | B | legacy | 50,389,190 | 167,964 |
| 5 | B | subscribe(BTC) | 49,544,942 | 165,150 |

## 원본 출력

### 측정 1 (A, REST)

```
checks_total.......: 103103  156.45685/s
checks_succeeded...: 100.00% 103103 out of 103103
http_req_duration..: avg=14.4s  min=3.72ms   med=17.03s max=18.7s  p(90)=17.84s p(95)=18.19s
http_req_failed....: 0.00%  0 out of 109103
http_reqs..........: 109103 165.561737/s
iterations.........: 103103 156.45685/s
vus_max............: 3000   min=2129  max=3000
```

### 측정 2 (A + WS legacy N=300)

REST:
```
checks_total.......: 78578   119.102346/s
checks_succeeded...: 100.00% 78578 out of 78578
http_req_duration..: avg=18.68s min=4.57ms   med=23.19s max=25.47s p(90)=24.52s p(95)=24.81s
http_req_failed....: 0.00% 0 out of 84578
http_reqs..........: 84578 128.196673/s
iterations.........: 78578 119.102346/s
```

WS:
```
checks_total...........: 396     0.64915/s (100% succeeded)
ws_connected_total......: 696     1.14093/s
ws_messages_received....: 29866208 48958.701618/s
ws_session_duration.....: avg=1m59s med=1m32s max=9m30s
data_received............: 5.8 GB   9.6 MB/s
```

### 측정 3 (B, REST) — load-gen 리사이즈 후 재실행

```
checks_total.......: 554469  861.061649/s
checks_succeeded...: 100.00% 554469 out of 554469
http_req_duration..: avg=2.48s min=4.04ms   med=2.64s max=5.8s  p(90)=2.85s p(95)=2.94s
http_req_failed....: 0.00%  0 out of 560469
http_reqs..........: 560469 870.379339/s
iterations.........: 554469 861.061649/s
vus_max............: 3000   min=3000  max=3000
```

최초 시도(OOM으로 중단, 참고용): 277초 경과 시점까지 144,811 iterations 누적, 서버 응답시간 8~18ms로 정상. `dmesg`: `Out of memory: Killed process 3043 (k6) total-vm:9132220kB, anon-rss:7780520kB`.

### 측정 4 (B + WS legacy N=300)

REST:
```
checks_total.......: 368494  572.440256/s
checks_succeeded...: 100.00% 368494 out of 368494
http_req_duration..: avg=3.88s min=3.92ms   med=4.38s max=6.51s p(90)=4.62s p(95)=4.72s
http_req_failed....: 0.00%  0 out of 374494
http_reqs..........: 374494 581.761009/s
iterations.........: 368494 572.440256/s
```

WS:
```
checks_total...........: 300     0.491799/s (100% succeeded)
ws_connected_total......: 600     0.983597/s
ws_messages_received....: 50389190 82604.464203/s
ws_session_duration.....: avg=9m30s (전원 만료까지 연결 유지)
data_received............: 19 GB    31 MB/s
```

### 측정 5 (B + WS subscribe N=300, BTC)

REST:
```
checks_total.......: 362961  563.9777/s
checks_succeeded...: 100.00% 362961 out of 362961
http_req_duration..: avg=3.95s min=3.97ms  med=4.43s max=6.55s p(90)=4.65s p(95)=4.75s
http_req_failed....: 0.00%  0 out of 368961
http_reqs..........: 368961 573.300647/s
iterations.........: 362961 563.9777/s
```

WS:
```
checks_total...........: 300     0.491799/s (100% succeeded)
ws_connected_total......: 600     0.983599/s
ws_messages_received....: 49544942 81220.588804/s
ws_session_duration.....: avg=9m30s
data_received............: 18 GB    30 MB/s
```

## 해석

1. **B-1a(코얼레싱)+B-4(그룹커밋)의 REST 순수 효과는 +450%다(측정 3 vs 1).** p95도 18.19s→2.94s로 급감했다. A-3(17번, −17%)와 그 흡수(18번, +20.6%)가 되찾은 것보다 훨씬 큰 폭인데, 그룹커밋이 trade당 DB 왕복을 최대 32:1로 묶고(스냅샷 확인: 배치의 97%가 상한 32에 도달) 코얼레싱이 매칭 엔진의 스냅샷 생성 부하를 100ms당 1회로 캡핑한 두 효과가 겹쳤기 때문으로 보인다. **판정 기준(3 ≥ 1)을 크게 충족** — 회귀 이등분 절차는 필요 없었다.
2. **WS 결박은 절대적으로는 크게 완화됐지만, 상대적 하락폭은 오히려 커졌다.** A+WS(측정 2)는 REST를 −23.9% 깎았고, B+WS(측정 4)는 −33.5% 깎았다 — **4/3 비율(0.665)이 2/1 비율(0.761)보다 낮아, 계획서가 세운 "결박 해소 실증" 기준(4/3 > 2/1)은 충족하지 못했다.** 다만 절대 처리량은 B+WS(572 iter/s)가 A+WS(119 iter/s)의 4.8배로 여전히 압도적이다. 원인으로 추정되는 것: B-1a 코얼레싱은 심볼별 스냅샷 생성 빈도를 100ms당 1회로 캡핑하지만, 체결(trade) 브로드캐스트 자체는 캡핑 대상이 아니다(정산 워커가 체결마다 발행) — B가 처리하는 trade 수 자체가 5배 이상 늘었으므로, legacy full-feed로 나가는 메시지 총량도 그만큼 늘어(측정 2: 2,986만 건 → 측정 4: 5,038만 건, +69%) 브로드캐스트 비용이 새로운 상대적 병목으로 부상한 것으로 보인다. 이는 계획서의 "3 < 1 회귀" 이등분 트리거에는 해당하지 않는 결과지만(3이 1보다 훨씬 높음), 정직하게 기록한다.
3. **B-1b(WS 구독)의 메시지 감소 효과는 이번 부하 프로파일로는 측정할 수 없었다.** 측정 5(subscribe)가 측정 4(legacy) 대비 받은 메시지가 49.5M vs 50.4M로 사실상 동일(−1.7%)했다. 원인은 `stress-hold3000.js`(그리고 원본 `loadtest/order-submission-stress.js`)가 **BTC 단일 심볼만 거래**하도록 고정돼 있기 때문 — legacy full-feed도 애초에 BTC 메시지밖에 없으므로 구독 필터링이 걸러낼 다른 심볼 트래픽이 존재하지 않는다. B-1b의 라우팅 로직 자체는 [5번 완료 문서](../refactor/5_B-1_스냅샷_코얼레싱과_WS_구독_완료.md)의 단위·E2E 테스트로 이미 검증됐으므로 기능적으로는 문제가 아니지만, **다중 심볼 부하 프로파일 없이는 GCP 수준에서 이 효과를 실측할 수 없다**는 것이 이번 세션의 결론이다.
4. **부하생성기가 새로운 병목이 될 수 있다는 것도 이번 세션의 교훈이다.** 서버가 충분히 빨라지면 고정 VU 수의 클라이언트가 스스로 자원을 소진할 수 있다(k6가 e2-standard-2에서 OOM). 서버 최적화가 누적될수록 향후 측정에서는 load-gen 용량도 같이 점검해야 한다.
5. **에러율은 5개 측정 전부 0%.** 그룹커밋 폴백(`settlement_batch_fallbacks_total`)도 3회 스냅샷 모두 0 — 배치 트랜잭션이 이 부하에서 한 번도 실패하지 않았다.

## 범위 밖 (Out of Scope)

- **B-1b 다중 심볼 측정**: 위 해석 3에서 설명한 한계. 여러 심볼을 실제로 거래하는 새 부하 프로파일 설계가 필요하며, 별도 작업이다.
- **WS 브로드캐스트 자체의 배치화/코얼레싱**: 해석 2에서 드러난 새로운 상대적 병목("체결 브로드캐스트는 코얼레싱 대상이 아님")의 해소는 이번 계획 범위 밖이며, 로드맵 후속 항목으로 고려할 만하다.
- 프론트엔드 구독 opt-in 적용 (별도 리포).
- 시장가 경로 벤치마크 (기존 프로파일이 지정가 전용 — 17번과 동일 스코프 유지).
- B-3(심볼 샤딩) 사전 측정 — 이번 결과(측정 3, 861 iter/s)가 그 기준선이 된다.

## 첨부: k6 스크립트 원문

### `~/stress-hold3000.js` (재구성 — `loadtest/order-submission-stress.js` 구조 기반, hold 프로파일)

```javascript
import http from 'k6/http';
import { check, sleep } from 'k6';
import exec from 'k6/execution';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const DEV_TOOLS_TOKEN = __ENV.DEV_TOOLS_TOKEN;
const DEV_TOOLS_TOKEN_HEADER = 'X-GoExchange-Dev-Token';

const TOTAL_USERS = 3000;
const SETUP_BATCH_SIZE = 100;
const COIN_SYMBOL = 'BTC';
const FIXED_PRICE = '50000000';
const ORDER_AMOUNT = '0.001';
const BUYER_KRW_FUNDING = '100000000';
const SELLER_BTC_FUNDING = '1000';

// steady-state 포화 프로파일: 1.5분 워밍업(0->3000) 후 3000에서 8분 유지.
const HOLD_STAGES = [
  { duration: '1.5m', target: 3000 },
  { duration: '8m', target: 3000 },
];

export const options = {
  setupTimeout: '20m',
  batch: SETUP_BATCH_SIZE,
  batchPerHost: SETUP_BATCH_SIZE,
  scenarios: {
    order_submission_hold: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: HOLD_STAGES,
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
  for (let batchStart = 1; batchStart <= TOTAL_USERS; batchStart += SETUP_BATCH_SIZE) {
    const batchEnd = Math.min(batchStart + SETUP_BATCH_SIZE - 1, TOTAL_USERS);
    const batchIndices = [];
    for (let i = batchStart; i <= batchEnd; i++) batchIndices.push(i);

    const registerRequests = batchIndices.map((i) => [
      'POST',
      `${BASE_URL}/auth/register`,
      JSON.stringify({
        name: `Stress Test User ${i}`,
        email: `stress-user-${i}@test.local`,
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
        throw new Error(`setup: failed to register user ${i}: ${res.status} ${res.body}`);
      }
    });

    if (loginNeeded.length > 0) {
      const loginRequests = loginNeeded.map((i) => [
        'POST',
        `${BASE_URL}/auth/login`,
        JSON.stringify({ email: `stress-user-${i}@test.local`, password: 'loadtest-password-123' }),
        { headers: { 'Content-Type': 'application/json' }, tags: { name: 'setup' } },
      ]);
      const loginResponses = http.batch(loginRequests);
      loginResponses.forEach((res, idx) => {
        const i = loginNeeded[idx];
        if (res.status !== 200) {
          throw new Error(`setup: user ${i} already registered but login failed: ${res.status} ${res.body}`);
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
        throw new Error(`setup: failed to fund wallet for user ${i}: ${res.status} ${res.body}`);
      }
      const role = i % 2 === 1 ? 'buyer' : 'seller';
      users.push({ token: tokensByIndex[i], role });
    });
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
```

### `~/ws-load.js` (신규)

```javascript
import ws from 'k6/ws';
import { Counter } from 'k6/metrics';
import { check } from 'k6';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const WS_URL = BASE_URL.replace(/^http/, 'ws') + '/ws';
const MODE = __ENV.MODE || 'legacy'; // legacy | subscribe
const N = parseInt(__ENV.N || '300', 10);
const DURATION = __ENV.DURATION || '9m30s';
const HOLD_MS = parseInt(__ENV.HOLD_MS || '570000', 10); // 9m30s

const wsMessagesReceived = new Counter('ws_messages_received');
const wsConnected = new Counter('ws_connected_total');

export const options = {
  scenarios: {
    ws_load: {
      executor: 'constant-vus',
      vus: N,
      duration: DURATION,
      exec: 'wsClient',
    },
  },
};

// 서버 ServeWs는 Origin 체크가 있다 - 기본 허용 목록(http://localhost:3000)에 있는
// Origin을 명시해 서버 env 변경 없이 연결을 통과시킨다.
export function wsClient() {
  const params = { headers: { Origin: 'http://localhost:3000' } };
  const res = ws.connect(WS_URL, params, function (socket) {
    socket.on('open', function () {
      wsConnected.add(1);
      if (MODE === 'subscribe') {
        socket.send(JSON.stringify({ action: 'subscribe', coin_symbols: ['BTC'] }));
      }
    });
    socket.on('message', function () {
      wsMessagesReceived.add(1);
    });
    socket.setTimeout(function () {
      socket.close();
    }, HOLD_MS);
  });
  check(res, { 'ws connected (status 101)': (r) => r && r.status === 101 });
}
```
