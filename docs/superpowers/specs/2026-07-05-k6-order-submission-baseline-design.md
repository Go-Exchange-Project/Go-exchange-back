# k6 주문 제출 API 부하테스트 (기준선) 설계

## 배경

`internal/matching` 패키지의 동시성/벤치마크 테스트(이전 스펙: `2026-07-05-matching-engine-concurrency-benchmark-tests-design.md`)는 매칭엔진 내부 로직만 측정했다. 이는 HTTP API, 인증 미들웨어, DB 쓰기(주문 저장, 잔고 차감, 트레이드/정산), 직렬화, 네트워크를 전혀 포함하지 않는다. 이번 작업은 실제 API 엔드포인트를 대상으로 k6 부하테스트를 만들어, 시스템 전체(API + DB 포함) 관점에서의 처리량/지연시간 기준선을 잡는 것이 목표다.

업계에서는 보통 smoke → load(기준선) → stress → soak → mixed 순서로 부하테스트를 단계적으로 만든다. 이번 작업은 그중 **load(기준선) 테스트 하나**만 다루며, 가장 핵심적인 쓰기 경로인 **주문 제출(`POST /orders`)**을 대상으로 한다. 스트레스 테스트(VU를 한계까지 올리는 것)와 혼합 시나리오(조회 트래픽 포함)는 이 기준선이 나온 뒤 별도 브레인스토밍 주제로 다룬다.

## 범위

- k6 스크립트 1개(`loadtest/order-submission-baseline.js`)와 실행 방법을 담은 `loadtest/README.md`를 새로 만든다.
- 테스트 대상 엔드포인트는 `POST /orders` (주문 생성)이며, 인증(`POST /auth/register`)과 지갑 충전(`POST /dev/wallets/fund`)은 준비 단계(setup)에서만 사용한다.
- 로컬 환경(`docker-compose.test.yml`의 Postgres + `go run cmd/main.go`로 기동한 백엔드)을 대상으로 한다. EC2 배포 환경은 범위 밖이다.
- VU(가상 유저) 규모는 10~50명의 소규모로 시작한다.
- 임계값(threshold)은 걸지 않는다 — 이번 목표는 "현재 시스템이 어느 정도 성능을 내는지 기준선을 재는 것"이며, 통과/실패 기준은 이 기준선이 나온 뒤 정한다.

## 아키텍처

### 1. 파일 구조

- `loadtest/order-submission-baseline.js` — k6 스크립트 본체 (setup + 시나리오)
- `loadtest/README.md` — 실행 방법 문서

지금은 시나리오가 하나뿐이므로 폴더를 더 세분화하지 않는다. 향후 혼합 시나리오/스트레스 테스트가 추가되면 이 디렉토리 안에 파일을 늘리는 정도로 확장한다.

### 2. Setup 단계 (유저/지갑 준비)

`setup()`은 테스트 실행마다 한 번만 실행되며, 이후 모든 VU가 공유할 데이터를 만든다.

1. 최대 VU 수(50명)만큼 유저를 미리 등록한다: `POST /auth/register`로 `loadtest-user-{i}@test.local` 형식 이메일과 고정 비밀번호를 사용해 50명 순차 가입. `Register` 응답에 이미 JWT `token`이 포함되므로 새 유저인 경우 별도 로그인 호출은 필요 없다. 다만 같은 테스트 DB에 스크립트를 여러 번 재실행하면 이미 등록된 이메일(`409`)을 만나는데, 이 경우 `POST /auth/login`으로 폴백해 토큰을 받는다 (재실행 가능성을 위한 구현 세부사항).
2. 역할을 번갈아 배정한다: 홀수 번째는 "매수자(buyer)", 짝수 번째는 "매도자(seller)" — VU ID가 순서대로 배정되는 특성상, 소규모(스모크) 테스트에서도 매수자/매도자가 함께 활성화되도록 한다.
3. `POST /dev/wallets/fund`(개발자 도구 엔드포인트, `X-Dev-Tools-Token` 헤더 필요)로 지갑을 충전한다:
   - 매수자: `coin_symbol: "KRW"`, 넉넉한 금액(예: `100000000`)
   - 매도자: `coin_symbol: "BTC"`, 넉넉한 수량(예: `1000`)
4. `setup()`은 `{ users: [{ token, role }, ...] }`를 반환하며, 이 데이터가 모든 VU의 시나리오 함수에 전달된다.

각 VU는 `exec.vu.idInTest`(1부터 시작하는 고유 VU 번호)로 `users` 배열에서 자기 자신에게 배정된 유저를 결정론적으로 골라 쓴다. 즉 같은 유저 토큰을 여러 VU가 동시에 쓰는 일이 없다.

`DEV_TOOLS_TOKEN`과 API 베이스 URL(기본값 `http://localhost:8080`)은 k6 `__ENV`로 주입받는 환경변수로 둔다.

### 3. 부하 시나리오 (주문 제출 로직)

- **대상 심볼/가격**: 단일 심볼 `BTC`, 고정 체결가 `50000000`(5천만원) — 매수/매도가 항상 같은 가격에 걸리므로 반드시 크로스되어 체결이 발생한다.
- **VU 동작**:
  - `role === 'buyer'`: `POST /orders`로 `{side: "BUY", order_type: "LIMIT", coin_symbol: "BTC", price: "50000000", amount: "0.001"}` 제출
  - `role === 'seller'`: 같은 가격, `side: "SELL"`로 제출
  - 매 요청 뒤 0.2~0.5초 사이 랜덤 sleep을 둬서 실제 유저 페이스를 흉내낸다. 스트레스 테스트 단계에서는 이 sleep을 줄이거나 없애면 된다.
- **부하 프로파일 (ramping-vus executor)**:
  ```
  stages: [
    { duration: '30s', target: 10 },  // 웜업
    { duration: '1m',  target: 50 }, // 목표까지 램프업
    { duration: '1m',  target: 50 }, // 정상상태 유지
    { duration: '20s', target: 0 },  // 램프다운
  ]
  ```
- **체결 보장 근거**: 양쪽 역할이 같은 가격에 끊임없이 주문을 넣으므로, 초반 몇 번의 반복 이후엔 항상 반대편에 체결 가능한 대기 주문이 남아있어 대부분의 주문이 즉시(또는 거의 즉시) 체결된다. 매수자/매도자가 서로 다른 유저이므로 셀프 트레이드 방지 로직에 걸리지 않는다.
- **메트릭 태깅**: `POST /orders` 요청에 `tags: { name: 'create_order' }`를 붙여 k6 요약에서 이 요청만 따로 걸러본다. `setup()`의 가입/충전 요청은 `tags: { name: 'setup' }`로 분리해 메인 지표에 섞이지 않게 한다.
- **체결 여부**: 주문은 실제로 체결되도록 구성한다 (오더북에 쌓이기만 하는 게 아님). 주문 저장 → 매칭 → 체결(settlement) → 지갑/원장 업데이트까지 전체 경로를 타서, 실제 프로덕션과 가장 비슷한 부하를 준다.

### 4. 결과 기록

기존에 정한 벤치마크 기록 컨벤션(`docs/benchmarks/NN-YYYY-MM-DD-...md`, 순번+날짜, `docs/benchmarks/README.md` 참고)을 그대로 따른다.

1. k6 실행이 끝나면 터미널에 출력되는 기본 요약(iterations, VUs, `http_req_duration` avg/p95/p99, `http_req_failed` 등)을 그대로 복사해 `docs/benchmarks/NN-YYYY-MM-DD-k6-order-submission-baseline.md`에 "원본 출력"으로 남긴다.
2. 커밋 해시, 실행 커맨드, 요약 테이블, 해석(임계값 없이 "현재 기준선" 기록)을 포함한다.
3. `docs/benchmarks/README.md`의 기록 목록에 한 줄 추가한다.

`--summary-export`로 JSON을 남기는 옵션도 있지만, 이번엔 raw JSON까지 커밋하지 않는다 (필요해지면 나중에 추가).

### 5. 실행 방법

1. **k6 설치** — 이 머신엔 아직 없으므로 설치가 필요하다.
2. **DB 기동**: `docker-compose -f docker-compose.test.yml up -d`
3. **환경변수 확인**: `.env.local`에 `GOEXCHANGE_ENABLE_DEV_TOOLS=true`, `GOEXCHANGE_DEV_TOOLS_TOKEN=<값>` 설정.
4. **서버 기동**: 별도 터미널에서 `go run cmd/main.go`.
5. **k6 실행**: `k6 run -e BASE_URL=http://localhost:8080 -e DEV_TOOLS_TOKEN=<값> loadtest/order-submission-baseline.js`
6. 결과를 위 컨벤션대로 `docs/benchmarks/`에 기록.

## 성공 기준

특정 성능 수치(예: p95 < Nms)를 목표로 하지 않는다. 이번 작업의 목표는 "API+DB를 포함한 주문 제출 경로의 현재 기준선을 측정 가능하게 만드는 것"이다.

- k6 스크립트가 에러 없이 완료되고, `create_order` 태그로 걸러본 `http_req_duration`(avg/p95/p99)과 `http_req_failed` 비율이 출력된다.
- 실제 체결(트레이드)이 발생했음을 확인할 수 있다 (예: 테스트 종료 후 `GET /trades`로 확인하거나, 서버 로그의 정산 처리 로그로 확인).
- 결과가 `docs/benchmarks/`에 기록된다.

## 범위 밖 (Out of Scope)

- 스트레스 테스트(한계점 탐색), 혼합 시나리오(조회 트래픽 포함) — 이번 기준선이 나온 뒤 별도 브레인스토밍 주제
- EC2 배포 환경 대상 테스트
- 임계값(threshold) 설정 및 CI 연동
- k6 결과의 raw JSON 커밋
