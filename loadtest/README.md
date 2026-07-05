# k6 부하테스트

`POST /orders` 엔드포인트를 대상으로 하는 k6 부하테스트. 인증, DB 쓰기, 매칭엔진 제출,
정산까지 포함한 실제 API 경로 전체를 측정한다.

## 사전 준비

1. k6 설치 확인: `k6 version` (없으면 `winget install --id=GrafanaLabs.k6 -e`). 설치 직후에는 현재 셸이 갱신된 PATH를 못 읽을 수 있으니, 새 터미널을 열거나 `export PATH="/c/Program Files/k6:$PATH"`로 직접 잡아준다.
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
- 같은 테스트 DB에 스크립트를 여러 번 실행해도 안전하다: `setup()`이 이미 등록된 유저를
  만나면(`409`) 자동으로 로그인으로 폴백한다. 다만 이 경우 k6의 전역 `http_req_failed`
  지표에는 이 예상된 `409`가 실패로 잡히므로, 실패율은 `create_order` 태그 기준으로
  판단해야 한다.
