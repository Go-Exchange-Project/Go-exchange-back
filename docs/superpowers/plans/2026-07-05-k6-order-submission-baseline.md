# k6 주문 제출 API 부하테스트 (기준선) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `POST /orders` 엔드포인트(인증, DB 쓰기, 매칭, 정산 전체 경로)를 대상으로 하는 k6 부하테스트 스크립트를 만들고, 로컬 환경에서 VU 10~50명 규모의 기준선 결과를 측정해 `docs/benchmarks/`에 기록한다.

**Architecture:** k6 스크립트의 `setup()`이 매수자 25명/매도자 25명을 가입시키고 지갑을 충전한 뒤, `ramping-vus` executor로 VU를 10→50명까지 올리며 고정 체결가에 매수/매도 주문을 계속 제출해 실제 체결이 발생하게 한다. 대상 DB는 `docker-compose.test.yml`의 격리된 테스트 Postgres이며, 서버 실행 시 환경변수로 접속 정보를 오버라이드한다.

**Tech Stack:** k6 (JavaScript), Gin 백엔드(`cmd/main.go`), Postgres(`docker-compose.test.yml`), 기존 `docs/benchmarks/` 기록 컨벤션.

## Global Constraints

- 대상 DB: `docker-compose.test.yml`의 테스트 Postgres — 호스트 `localhost`, 포트 `55432`, 유저/DB명 `goexchange_test`, 비밀번호 `goexchange_test_password`. **평소 로컬 개발용 DB(`localhost:5432/goexchange`)와는 절대 섞이지 않아야 한다.**
- 서버 실행 시 `GOEXCHANGE_DB_HOST`, `GOEXCHANGE_DB_PORT`, `GOEXCHANGE_DB_USER`, `GOEXCHANGE_DB_NAME`, `GOEXCHANGE_DB_PASSWORD`를 셸 환경변수로 명시적으로 오버라이드한다 (`.env.local` 파일은 수정하지 않는다 — `config.LoadLocalEnvFiles()`는 이미 설정된 OS 환경변수를 덮어쓰지 않으므로 셸에서 먼저 export하면 `.env.local`보다 우선한다).
- 개발자 도구 토큰: `.env.local`에 이미 `GOEXCHANGE_ENABLE_DEV_TOOLS=true`, `GOEXCHANGE_DEV_TOOLS_TOKEN=local-dev-token`이 설정되어 있다. 헤더명은 `X-GoExchange-Dev-Token` (`internal/middleware/dev_tools.go`의 `DevToolsTokenHeader` 상수).
- 대상 심볼: `BTC`, 고정 체결가 `50000000`, 주문 수량 `0.001` (전량 지정가). `config/market_rules.json`에서 `BTC`는 `ACTIVE`, `min_order_quantity`/`base_quantity_step` = `0.00000001`이라 이 값들은 유효하다.
- 테스트 유저 50명: 앞 25명은 `buyer`(코인 심볼 `KRW`로 `100000000` 충전), 뒤 25명은 `seller`(코인 심볼 `BTC`로 `1000` 충전). `KRW` 자산 심볼 상수는 `model.KRWAssetSymbol = "KRW"`.
- VU 프로파일: `ramping-vus` executor, `stages: [{30s→10}, {1m→50}, {1m@50}, {20s→0}]`.
- 임계값(threshold)은 걸지 않는다.
- 새로 만드는 파일은 정확히 2개: `loadtest/order-submission-baseline.js`, `loadtest/README.md`. 그 외에 `docs/benchmarks/`에 결과 기록 파일 1개와 `docs/benchmarks/README.md` 갱신이 추가된다.
- 범위 밖: 스트레스/혼합 시나리오, EC2 배포 환경 대상 테스트, 임계값/CI 연동, k6 결과 raw JSON 커밋.

---

### Task 1: 로컬 테스트 환경 기동 및 검증

**Files:** 없음 (환경 검증만 수행, 파일 생성/수정 없음)

**Interfaces:**
- Consumes: `docker-compose.test.yml` (기존 파일), `cmd/main.go` (기존 서버 엔트리포인트), `.env.local`의 `GOEXCHANGE_ENABLE_DEV_TOOLS`/`GOEXCHANGE_DEV_TOOLS_TOKEN`
- Produces: 이후 태스크들이 재사용할 "테스트 DB 컨테이너 + 로컬 서버가 정상 기동된 상태" (포트 8080에서 서버가 테스트 DB를 보고 있음)

- [ ] **Step 1: 테스트 Postgres 컨테이너 기동**

Run: `cd "C:\Users\dksco\OneDrive\Desktop\GoExchange\Go-exchange-back" && docker-compose -f docker-compose.test.yml up -d`

Expected: `goexchange-postgres-test` 컨테이너가 생성되고 시작됨

- [ ] **Step 2: 컨테이너 헬스체크 확인**

Run: `docker ps --filter "name=goexchange-postgres-test" --format "{{.Names}}: {{.Status}}"`

Expected: 상태에 `(healthy)`가 표시될 때까지 몇 초 대기 후 재확인 (최초 `starting`일 수 있음)

- [ ] **Step 3: k6 설치 여부 확인, 없으면 설치**

Run: `where k6` (Windows) 또는 `k6 version`

Expected: 설치되어 있지 않으면(`찾을 수 없음`/명령어 없음 에러) 다음으로 설치:
```
winget install --id=k6.k6 -e --accept-source-agreements --accept-package-agreements
```
설치 후 새 셸에서 `k6 version`을 실행해 버전 문자열이 출력되는지 확인한다.

- [ ] **Step 4: 테스트 DB를 바라보도록 환경변수를 설정하고 서버를 백그라운드로 기동**

Run (하나의 셸 세션에서 순서대로):
```bash
cd "C:\Users\dksco\OneDrive\Desktop\GoExchange\Go-exchange-back"
export GOEXCHANGE_DB_HOST=localhost
export GOEXCHANGE_DB_PORT=55432
export GOEXCHANGE_DB_USER=goexchange_test
export GOEXCHANGE_DB_NAME=goexchange_test
export GOEXCHANGE_DB_PASSWORD=goexchange_test_password
nohup go run cmd/main.go > /tmp/goexchange-server.log 2>&1 &
disown
echo "server PID: $!"
```

`nohup`과 `disown`을 반드시 함께 쓴다 — 이 서버는 이 태스크의 셸 세션이 끝난 뒤에도(다음 태스크가 별도 프로세스/서브에이전트로 실행되더라도) 계속 살아있어야 하기 때문이다. 일반 `&`만으로는 셸 세션이 종료될 때 함께 죽을 수 있다.

Expected: 백그라운드 PID가 출력됨. 몇 초 후 `/tmp/goexchange-server.log`에 `DB connection established`와 `matching bootstrap completed`가 찍혀 있어야 한다.

- [ ] **Step 5: 서버 응답 확인**

Run: `curl -s http://localhost:8080/ping`

Expected: `{"data":{"message":"pong"}}`

- [ ] **Step 6: 개발자 도구 엔드포인트까지 엔드투엔드로 확인 (임시 유저로 가입 → 충전)**

Run:
```bash
curl -s -X POST http://localhost:8080/auth/register \
  -H "Content-Type: application/json" \
  -d '{"name":"Env Check","email":"env-check@test.local","password":"env-check-pass-123"}'
```
응답 JSON의 `data.token` 값을 `TOKEN` 변수에 담은 뒤:
```bash
curl -s -X POST http://localhost:8080/dev/wallets/fund \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -H "X-GoExchange-Dev-Token: local-dev-token" \
  -d '{"coin_symbol":"KRW","amount":"1000"}'
```

Expected: 두 응답 모두 200/201과 함께 `data` 필드에 정상 값 반환 (`"message":"wallet funded"` 등). 에러가 나면 이 태스크를 완료로 표시하지 말고 원인(포트 충돌, 컨테이너 미기동, 토큰 오타 등)을 먼저 해결한다.

**서버와 DB는 이후 태스크(Task 2, 4)에서도 계속 사용하므로 끄지 않고 그대로 둔다.**

---

### Task 2: k6 스크립트 작성 (`order-submission-baseline.js`) 및 스모크 테스트

**Files:**
- Create: `loadtest/order-submission-baseline.js`

**Interfaces:**
- Consumes: Task 1에서 기동한 로컬 서버(`http://localhost:8080`), 개발자 도구 헤더 `X-GoExchange-Dev-Token`
- Produces: `setup()`이 반환하는 `{ users: [{ token: string, role: 'buyer'|'seller' }, ...] }` 구조 — Task 4의 전체 실행에서도 그대로 재사용

- [ ] **Step 1: 스크립트 작성**

`loadtest/order-submission-baseline.js`:

```javascript
import http from 'k6/http';
import { check, sleep } from 'k6';
import exec from 'k6/execution';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const DEV_TOOLS_TOKEN = __ENV.DEV_TOOLS_TOKEN;
const DEV_TOOLS_TOKEN_HEADER = 'X-GoExchange-Dev-Token';

const TOTAL_USERS = 50;
const BUYER_COUNT = 25;
const COIN_SYMBOL = 'BTC';
const FIXED_PRICE = '50000000';
const ORDER_AMOUNT = '0.001';
const BUYER_KRW_FUNDING = '100000000';
const SELLER_BTC_FUNDING = '1000';

const FULL_STAGES = [
  { duration: '30s', target: 10 },
  { duration: '1m', target: 50 },
  { duration: '1m', target: 50 },
  { duration: '20s', target: 0 },
];

const SMOKE_STAGES = [
  { duration: '5s', target: 2 },
  { duration: '10s', target: 2 },
  { duration: '5s', target: 0 },
];

export const options = {
  scenarios: {
    order_submission_baseline: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: __ENV.SMOKE_TEST === 'true' ? SMOKE_STAGES : FULL_STAGES,
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
  for (let i = 1; i <= TOTAL_USERS; i++) {
    const role = i <= BUYER_COUNT ? 'buyer' : 'seller';
    const email = `loadtest-user-${i}@test.local`;

    const registerRes = http.post(
      `${BASE_URL}/auth/register`,
      JSON.stringify({
        name: `Load Test User ${i}`,
        email: email,
        password: 'loadtest-password-123',
      }),
      { headers: { 'Content-Type': 'application/json' }, tags: { name: 'setup' } }
    );

    if (registerRes.status !== 201) {
      throw new Error(
        `setup: failed to register user ${i} (${email}): ${registerRes.status} ${registerRes.body}`
      );
    }
    const token = registerRes.json('data.token');

    const fundBody =
      role === 'buyer'
        ? { coin_symbol: 'KRW', amount: BUYER_KRW_FUNDING }
        : { coin_symbol: COIN_SYMBOL, amount: SELLER_BTC_FUNDING };

    const fundRes = http.post(`${BASE_URL}/dev/wallets/fund`, JSON.stringify(fundBody), {
      headers: {
        'Content-Type': 'application/json',
        Authorization: `Bearer ${token}`,
        [DEV_TOOLS_TOKEN_HEADER]: DEV_TOOLS_TOKEN,
      },
      tags: { name: 'setup' },
    });

    if (fundRes.status !== 200) {
      throw new Error(
        `setup: failed to fund wallet for user ${i} (${email}): ${fundRes.status} ${fundRes.body}`
      );
    }

    users.push({ token, role });
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

- [ ] **Step 2: 스모크 테스트 실행 (Task 1에서 기동한 서버가 계속 떠 있어야 함)**

Run:
```bash
cd "C:\Users\dksco\OneDrive\Desktop\GoExchange\Go-exchange-back"
k6 run -e BASE_URL=http://localhost:8080 -e DEV_TOOLS_TOKEN=local-dev-token -e SMOKE_TEST=true loadtest/order-submission-baseline.js
```

Expected: `setup()`이 에러 없이 50명 유저 등록/충전을 마치고, `checks` 요약에 `order accepted (status 200)` 100% 통과, `http_req_failed` 비율 0.00%로 출력된다.

- [ ] **Step 3: 실제 체결이 일어났는지 확인**

Run:
```bash
curl -s -X POST http://localhost:8080/auth/login \
  -H "Content-Type: application/json" \
  -d '{"email":"loadtest-user-1@test.local","password":"loadtest-password-123"}'
```
응답의 `data.token`으로:
```bash
curl -s http://localhost:8080/trades -H "Authorization: Bearer $TOKEN"
```

Expected: `data.trades` 배열에 1건 이상의 트레이드가 들어있음 (스모크 테스트만으로도 매수자 1번 유저가 최소 한 번은 체결됐어야 한다).

- [ ] **Step 4: 커밋**

```bash
git add loadtest/order-submission-baseline.js
git commit -m "feat: add k6 order-submission baseline load test script"
```

---

### Task 3: 실행 방법 문서화 (`loadtest/README.md`)

**Files:**
- Create: `loadtest/README.md`

**Interfaces:**
- Consumes: Task 1의 환경 기동 절차, Task 2의 스크립트 실행 커맨드
- Produces: 없음 (문서 파일)

- [ ] **Step 1: README 작성**

`loadtest/README.md`:

```markdown
# k6 부하테스트

`POST /orders` 엔드포인트를 대상으로 하는 k6 부하테스트. 인증, DB 쓰기, 매칭엔진 제출,
정산까지 포함한 실제 API 경로 전체를 측정한다.

## 사전 준비

1. k6 설치 확인: `k6 version` (없으면 `winget install --id=k6.k6 -e`)
2. 테스트 전용 Postgres 기동 (평소 개발용 DB와 분리됨):
   ```bash
   docker-compose -f docker-compose.test.yml up -d
   ```
3. 테스트 DB를 바라보도록 환경변수를 설정하고 서버 기동:
   ```bash
   export GOEXCHANGE_DB_HOST=localhost
   export GOEXCHANGE_DB_PORT=55432
   export GOEXCHANGE_DB_USER=goexchange_test
   export GOEXCHANGE_DB_NAME=goexchange_test
   export GOEXCHANGE_DB_PASSWORD=goexchange_test_password
   go run cmd/main.go
   ```
   `.env.local`에 `GOEXCHANGE_ENABLE_DEV_TOOLS=true`와 `GOEXCHANGE_DEV_TOOLS_TOKEN`이
   설정되어 있어야 한다 (개발자 도구 엔드포인트로 테스트 지갑을 충전하기 위함).

## 실행

**스모크 테스트** (몇 초, VU 2명 — 스크립트가 제대로 동작하는지만 확인):
```bash
k6 run -e BASE_URL=http://localhost:8080 -e DEV_TOOLS_TOKEN=<GOEXCHANGE_DEV_TOOLS_TOKEN 값> -e SMOKE_TEST=true loadtest/order-submission-baseline.js
```

**전체 기준선 테스트** (VU 10→50명, 약 2분 40초):
```bash
k6 run -e BASE_URL=http://localhost:8080 -e DEV_TOOLS_TOKEN=<GOEXCHANGE_DEV_TOOLS_TOKEN 값> loadtest/order-submission-baseline.js
```

## 결과 기록

실행이 끝나면 터미널에 출력된 k6 요약(특히 `create_order` 태그의 `http_req_duration`,
`http_req_failed`)을 그대로 복사해 `docs/benchmarks/` 컨벤션(`docs/benchmarks/README.md`
참고)대로 `NN-YYYY-MM-DD-k6-order-submission-baseline.md` 파일에 기록한다.

## 참고

- 대상 DB는 `docker-compose.test.yml`의 격리된 테스트 인스턴스다. 평소 로컬 개발용
  DB(`localhost:5432/goexchange`)와는 별개이므로, 이 부하테스트가 실제 개발 데이터를
  건드리지 않는다.
- 이 스크립트는 매수/매도 주문이 반드시 체결되도록 고정 가격(BTC, 5천만원)으로 설계되어
  있다. 실제 체결(트레이드) 발생 여부는 `GET /trades`로 확인할 수 있다.
- 이번 버전은 처리량/지연시간 "기준선" 측정이 목적이며 임계값(pass/fail threshold)은
  걸려있지 않다. 스트레스 테스트, 혼합 시나리오(조회 트래픽 포함) 는 별도 작업이다.
```

- [ ] **Step 2: 커밋**

```bash
git add loadtest/README.md
git commit -m "docs: add k6 load test run instructions"
```

---

### Task 4: 전체 기준선 실행 및 결과 기록

**Files:**
- Create: `docs/benchmarks/NN-YYYY-MM-DD-k6-order-submission-baseline.md` (NN과 날짜는 Step 1에서 결정한 실제 값으로 치환)
- Modify: `docs/benchmarks/README.md`

**Interfaces:**
- Consumes: Task 1의 실행 중인 서버/DB, Task 2의 `loadtest/order-submission-baseline.js`
- Produces: 없음 (최종 산출물)

- [ ] **Step 1: 다음 순번과 오늘 날짜 확인**

Run:
```bash
ls "C:\Users\dksco\OneDrive\Desktop\GoExchange\Go-exchange-back\docs\benchmarks"
date +%Y-%m-%d
```

Expected: 기존에 `01-2026-07-05-matching-engine-benchmarks.md`가 있으므로 이번 파일명은 `02-<오늘날짜>-k6-order-submission-baseline.md`가 된다.

- [ ] **Step 2: Task 1의 서버/DB가 계속 떠 있는지 확인**

Run: `curl -s http://localhost:8080/ping`

Expected: `{"data":{"message":"pong"}}`. 만약 서버가 죽어 있다면 Task 1의 Step 1, 4를 다시 수행해 재기동한다.

- [ ] **Step 3: 전체 기준선 테스트 실행 (약 2분 40초 소요, 전체 출력을 보존)**

Run:
```bash
cd "C:\Users\dksco\OneDrive\Desktop\GoExchange\Go-exchange-back"
k6 run -e BASE_URL=http://localhost:8080 -e DEV_TOOLS_TOKEN=local-dev-token loadtest/order-submission-baseline.js | tee /tmp/k6-baseline-output.txt
```

Expected: 에러 없이 완료되고, 요약에 `iterations`, `vus_max`, `http_req_duration`(avg/p95/p99), `http_req_failed` 수치가 출력된다.

- [ ] **Step 4: 실제 체결 확인**

Run:
```bash
curl -s -X POST http://localhost:8080/auth/login \
  -H "Content-Type: application/json" \
  -d '{"email":"loadtest-user-1@test.local","password":"loadtest-password-123"}'
```
응답의 `data.token`으로 `curl -s http://localhost:8080/trades -H "Authorization: Bearer $TOKEN"` 실행.

Expected: `data.trades`에 여러 건의 트레이드가 들어있음 (전체 테스트 기간 동안 다수 체결됐어야 함).

- [ ] **Step 5: 커밋 해시 확인**

Run: `git rev-parse --short HEAD`

- [ ] **Step 6: 결과 문서 작성**

`docs/benchmarks/02-YYYY-MM-DD-k6-order-submission-baseline.md` (파일명의 날짜는 Step 1에서 확인한 오늘 날짜로 치환):

```markdown
# k6 주문 제출 API 부하테스트 결과 — 2번째 테스트 (YYYY-MM-DD)

**커밋:** `<Step 5에서 확인한 짧은 해시>`
**대상:** `POST /orders` (인증 → DB 쓰기 → 매칭엔진 제출 → 정산 전체 경로)
**실행 커맨드:** `k6 run -e BASE_URL=http://localhost:8080 -e DEV_TOOLS_TOKEN=local-dev-token loadtest/order-submission-baseline.js`
**환경:** 로컬, `docker-compose.test.yml` 격리 테스트 Postgres, VU 10→50명 (ramping-vus)

## 원본 출력

```
<Step 3에서 저장한 /tmp/k6-baseline-output.txt 전체 내용을 그대로 붙여넣는다>
```

## 요약 테이블

| 지표 | 값 |
|---|---|
| 총 iterations | <k6 출력에서 확인> |
| 최대 VU | 50 |
| http_req_duration (create_order, avg) | <값> |
| http_req_duration (create_order, p95) | <값> |
| http_req_duration (create_order, p99 또는 max) | <값> |
| http_req_failed | <값> |

## 해석

<실제 수치를 보고 관찰한 내용을 적는다 — 예: p95가 어느 정도인지, 실패율이 0%인지,
Go 벤치마크(순수 매칭 로직, `docs/benchmarks/01-...md`)와 비교했을 때 API+DB 경로가
추가하는 오버헤드가 어느 정도인지>

## 재현 방법

`loadtest/README.md` 참고.
```

- [ ] **Step 7: `docs/benchmarks/README.md`의 기록 목록에 항목 추가**

`docs/benchmarks/README.md`의 "## 기록 목록" 아래에 한 줄 추가:
```markdown
- [02-YYYY-MM-DD-k6-order-submission-baseline.md](02-YYYY-MM-DD-k6-order-submission-baseline.md) — 2번째 테스트: POST /orders 부하테스트(k6) 기준선, VU 10~50명 (커밋 `<짧은 해시>`)
```

- [ ] **Step 8: 커밋 및 푸시**

```bash
git add docs/benchmarks/
git commit -m "docs: record k6 order-submission baseline load test results"
git push origin main
```

- [ ] **Step 9: 테스트 환경 정리**

Run:
```bash
kill %1 2>/dev/null || true
docker-compose -f docker-compose.test.yml down
```

(Task 1에서 백그라운드로 띄운 서버 프로세스를 종료하고, 테스트 DB 컨테이너를 내린다. 볼륨은 유지되므로 다음에 다시 `up -d`하면 데이터가 남아있다 — 완전히 지우려면 `down -v`를 쓰되, 이번 작업 범위에서는 필요하지 않다.)
