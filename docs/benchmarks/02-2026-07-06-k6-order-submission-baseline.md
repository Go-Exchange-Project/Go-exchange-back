# k6 주문 제출 API 부하테스트 결과 — 2번째 테스트 (2026-07-06)

**커밋:** `d2bf50a`
**대상:** `POST /orders` (인증 → DB 쓰기 → 매칭엔진 제출 → 정산 전체 경로)
**실행 커맨드:** `k6 run -e BASE_URL=http://localhost:8080 -e DEV_TOOLS_TOKEN=local-dev-token loadtest/order-submission-baseline.js`
**환경:** 로컬, `docker-compose.test.yml` 격리 테스트 Postgres, VU 10→50명 (ramping-vus)

## 원본 출력

```

          /\      Grafana   /‾‾/
     /\  /  \     |\  __   /  /
    /  \/    \    | |/ /  /   ‾‾\
   /          \   |   (  |  (‾)  |
  / __________ \  |_|\_\  \_____/


      execution: local
         script: loadtest/order-submission-baseline.js
         output: -

      scenarios: (100.00%) 1 scenario, 50 max VUs, 3m20s max duration (incl. graceful stop):
               * order_submission_baseline: Up to 50 looping VUs for 2m50s over 4 stages (gracefulRampDown: 30s, exec: submitOrders, gracefulStop: 30s)



   █ TOTAL RESULTS

     checks_total.......: 15075   87.06811/s
     checks_succeeded...: 100.00% 15075 out of 15075
     checks_failed......: 0.00%   0 out of 15075

     ✓ order accepted (status 200)

     HTTP
     http_req_duration..............: avg=10.33ms  min=506.9µs  med=6.43ms   max=268.14ms p(90)=23.1ms   p(95)=26.74ms
       { expected_response:true }...: avg=10.36ms  min=2.53ms   med=6.44ms   max=268.14ms p(90)=23.12ms  p(95)=26.77ms
     http_req_failed................: 0.32%  50 out of 15225
     http_reqs......................: 15225  87.93446/s

     EXECUTION
     iteration_duration.............: avg=359.87ms min=204.76ms med=359.66ms max=583.05ms p(90)=481.68ms p(95)=496.03ms
     iterations.....................: 15075  87.06811/s
     vus............................: 1      min=0           max=50
     vus_max........................: 50     min=50          max=50

     NETWORK
     data_received..................: 2.7 MB 16 kB/s
     data_sent......................: 5.9 MB 34 kB/s


running (2m53.1s), 00/50 VUs, 15075 complete and 0 interrupted iterations
order_submission_baseline ✓ [ 100% ] 00/50 VUs  2m50s
```

## 요약 테이블

| 지표 | 값 |
|---|---|
| 총 iterations | 15,075 |
| 최대 VU | 50 |
| http_req_duration (create_order, avg) | 10.33ms (전체 http_req_duration 기준; 아래 해석 참고) |
| http_req_duration (create_order, p95) | 26.74ms |
| http_req_duration (create_order, p99 또는 max) | max=268.14ms |
| http_req_failed | 0.32% (50/15,225) — 전부 `setup()`의 예상된 409 재등록 응답, `create_order` 자체 실패는 0건 |

## 해석

- k6 기본 텍스트 요약은 커스텀 태그(`name:create_order`)별로 지표를 분리해서 출력하지 않는다. 다만 `setup()`에서 태그된 요청(회원가입/로그인/지갑 충전, 50명 × 3건 = 150건)은 전체 15,225건 중 약 1%에 불과하고, 나머지 15,075건(약 99%)이 전부 `create_order` 요청이므로 위 `http_req_duration` 수치는 사실상 `create_order`의 성능을 그대로 반영한다.
- `http_req_failed`가 0.32%(50건)로 찍힌 것은 테스트 DB에 이전 스모크테스트에서 생성된 50명의 `loadtest-user-N@test.local` 계정이 이미 존재해서 `setup()`의 회원가입 요청이 전부 `409`를 반환했기 때문이다(k6는 4xx/5xx를 자동으로 `http_req_failed`에 집계함). 이는 Task 2에서 의도적으로 구현한 register-or-login 폴백 동작이며 버그가 아니다. 반면 부하테스트 본체인 `create_order` 요청에 대한 체크(`order accepted (status 200)`)는 15,075건 중 15,075건 전부 통과(100%)했다 — 즉 `create_order` 자체의 실패율은 0%다.
- p95 26.74ms, max 268.14ms 수준으로, 인증 검증 + DB 쓰기 + 매칭엔진 제출 + 정산까지 포함한 전체 API 경로가 매우 낮은 지연시간을 유지했다. 순수 매칭 로직만 측정한 Go 벤치마크(`docs/benchmarks/01-2026-07-05-matching-engine-benchmarks.md`)의 `BenchmarkMatch_ImmediateCross`는 약 1,576 ns(=0.0016ms)/op였으므로, API+DB+네트워크 왕복을 포함한 전체 경로는 순수 매칭 로직 대비 평균적으로 약 6,500배(10.33ms vs 0.0016ms), p95 기준으로는 약 17,000배의 오버헤드를 추가한다. 이는 예상된 결과로, 매칭엔진 자체는 마이크로초 단위로 매우 빠르지만 HTTP 요청/응답, JWT 인증, Postgres 트랜잭션(주문 생성, 지갑 잔고 확인/차감, 체결 기록, 정산)이 지연시간의 대부분을 차지한다는 것을 보여준다.
- 처리량은 평균 87.9 req/s(전체), 87.1 iterations/s로, VU가 50까지 램프업된 구간에서도 안정적으로 유지되었다.
- 실제 체결(트레이드) 검증: `loadtest-user-1@test.local`로 로그인 후 `GET /trades` 조회 결과, `engine_sequence`가 7,000번대 후반까지 올라간 다수의 체결 기록이 확인되었다(응답에 페이지네이션된 50건이 반환되었으며, `engine_sequence` 값 자체가 이번 실행 동안 최소 수천 건 이상의 체결이 발생했음을 보여줌). 매칭 및 정산 경로가 부하 상황에서도 정상적으로 동작했다.

## 재현 방법

`loadtest/README.md` 참고.
