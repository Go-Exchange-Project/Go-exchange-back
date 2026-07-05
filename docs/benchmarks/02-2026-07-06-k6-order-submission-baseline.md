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


Run                         [ 100% ] setup()
order_submission_baseline   [   0% ]

Run                         [ 100% ] setup()
order_submission_baseline   [   0% ]

running (0m03.0s), 00/50 VUs, 0 complete and 0 interrupted iterations
order_submission_baseline   [   0% ] 00/50 VUs  0m00.2s/2m50.0s

running (0m04.0s), 00/50 VUs, 0 complete and 0 interrupted iterations
order_submission_baseline   [   1% ] 00/50 VUs  0m01.2s/2m50.0s

running (0m05.0s), 00/50 VUs, 0 complete and 0 interrupted iterations
order_submission_baseline   [   1% ] 00/50 VUs  0m02.2s/2m50.0s

running (0m06.0s), 01/50 VUs, 0 complete and 0 interrupted iterations
order_submission_baseline   [   2% ] 01/50 VUs  0m03.2s/2m50.0s

running (0m07.0s), 01/50 VUs, 2 complete and 0 interrupted iterations
order_submission_baseline   [   2% ] 01/50 VUs  0m04.2s/2m50.0s

running (0m08.0s), 01/50 VUs, 5 complete and 0 interrupted iterations
order_submission_baseline   [   3% ] 01/50 VUs  0m05.2s/2m50.0s

running (0m09.0s), 02/50 VUs, 7 complete and 0 interrupted iterations
order_submission_baseline   [   4% ] 02/50 VUs  0m06.2s/2m50.0s

running (0m10.0s), 02/50 VUs, 14 complete and 0 interrupted iterations
order_submission_baseline   [   4% ] 02/50 VUs  0m07.2s/2m50.0s

running (0m11.0s), 02/50 VUs, 20 complete and 0 interrupted iterations
order_submission_baseline   [   5% ] 02/50 VUs  0m08.2s/2m50.0s

running (0m12.0s), 03/50 VUs, 25 complete and 0 interrupted iterations
order_submission_baseline   [   5% ] 03/50 VUs  0m09.2s/2m50.0s

running (0m13.0s), 03/50 VUs, 35 complete and 0 interrupted iterations
order_submission_baseline   [   6% ] 03/50 VUs  0m10.2s/2m50.0s

running (0m14.0s), 03/50 VUs, 44 complete and 0 interrupted iterations
order_submission_baseline   [   7% ] 03/50 VUs  0m11.2s/2m50.0s

running (0m15.0s), 04/50 VUs, 52 complete and 0 interrupted iterations
order_submission_baseline   [   7% ] 04/50 VUs  0m12.2s/2m50.0s

running (0m16.0s), 04/50 VUs, 63 complete and 0 interrupted iterations
order_submission_baseline   [   8% ] 04/50 VUs  0m13.2s/2m50.0s

running (0m17.0s), 04/50 VUs, 73 complete and 0 interrupted iterations
order_submission_baseline   [   8% ] 04/50 VUs  0m14.2s/2m50.0s

running (0m18.0s), 05/50 VUs, 84 complete and 0 interrupted iterations
order_submission_baseline   [   9% ] 05/50 VUs  0m15.2s/2m50.0s

running (0m19.0s), 05/50 VUs, 94 complete and 0 interrupted iterations
order_submission_baseline   [  10% ] 05/50 VUs  0m16.2s/2m50.0s

running (0m20.0s), 05/50 VUs, 108 complete and 0 interrupted iterations
order_submission_baseline   [  10% ] 05/50 VUs  0m17.2s/2m50.0s

running (0m21.0s), 06/50 VUs, 123 complete and 0 interrupted iterations
order_submission_baseline   [  11% ] 06/50 VUs  0m18.2s/2m50.0s

running (0m22.0s), 06/50 VUs, 141 complete and 0 interrupted iterations
order_submission_baseline   [  11% ] 06/50 VUs  0m19.2s/2m50.0s

running (0m23.0s), 06/50 VUs, 160 complete and 0 interrupted iterations
order_submission_baseline   [  12% ] 06/50 VUs  0m20.2s/2m50.0s

running (0m24.0s), 07/50 VUs, 177 complete and 0 interrupted iterations
order_submission_baseline   [  12% ] 07/50 VUs  0m21.2s/2m50.0s

running (0m25.0s), 07/50 VUs, 198 complete and 0 interrupted iterations
order_submission_baseline   [  13% ] 07/50 VUs  0m22.2s/2m50.0s

running (0m26.0s), 07/50 VUs, 217 complete and 0 interrupted iterations
order_submission_baseline   [  14% ] 07/50 VUs  0m23.2s/2m50.0s

running (0m27.0s), 08/50 VUs, 239 complete and 0 interrupted iterations
order_submission_baseline   [  14% ] 08/50 VUs  0m24.2s/2m50.0s

running (0m28.0s), 08/50 VUs, 262 complete and 0 interrupted iterations
order_submission_baseline   [  15% ] 08/50 VUs  0m25.2s/2m50.0s

running (0m29.0s), 08/50 VUs, 285 complete and 0 interrupted iterations
order_submission_baseline   [  15% ] 08/50 VUs  0m26.2s/2m50.0s

running (0m30.0s), 09/50 VUs, 305 complete and 0 interrupted iterations
order_submission_baseline   [  16% ] 09/50 VUs  0m27.2s/2m50.0s

running (0m31.0s), 09/50 VUs, 331 complete and 0 interrupted iterations
order_submission_baseline   [  17% ] 09/50 VUs  0m28.2s/2m50.0s

running (0m32.0s), 09/50 VUs, 357 complete and 0 interrupted iterations
order_submission_baseline   [  17% ] 09/50 VUs  0m29.2s/2m50.0s

running (0m33.0s), 10/50 VUs, 380 complete and 0 interrupted iterations
order_submission_baseline   [  18% ] 10/50 VUs  0m30.2s/2m50.0s

running (0m34.0s), 10/50 VUs, 409 complete and 0 interrupted iterations
order_submission_baseline   [  18% ] 10/50 VUs  0m31.2s/2m50.0s

running (0m35.0s), 11/50 VUs, 440 complete and 0 interrupted iterations
order_submission_baseline   [  19% ] 11/50 VUs  0m32.2s/2m50.0s

running (0m36.0s), 12/50 VUs, 471 complete and 0 interrupted iterations
order_submission_baseline   [  20% ] 12/50 VUs  0m33.2s/2m50.0s

running (0m37.0s), 12/50 VUs, 505 complete and 0 interrupted iterations
order_submission_baseline   [  20% ] 12/50 VUs  0m34.2s/2m50.0s

running (0m38.0s), 13/50 VUs, 544 complete and 0 interrupted iterations
order_submission_baseline   [  21% ] 13/50 VUs  0m35.2s/2m50.0s

running (0m39.0s), 14/50 VUs, 581 complete and 0 interrupted iterations
order_submission_baseline   [  21% ] 14/50 VUs  0m36.2s/2m50.0s

running (0m40.0s), 14/50 VUs, 620 complete and 0 interrupted iterations
order_submission_baseline   [  22% ] 14/50 VUs  0m37.2s/2m50.0s

running (0m41.0s), 15/50 VUs, 662 complete and 0 interrupted iterations
order_submission_baseline   [  22% ] 15/50 VUs  0m38.2s/2m50.0s

running (0m42.0s), 16/50 VUs, 701 complete and 0 interrupted iterations
order_submission_baseline   [  23% ] 16/50 VUs  0m39.2s/2m50.0s

running (0m43.0s), 16/50 VUs, 746 complete and 0 interrupted iterations
order_submission_baseline   [  24% ] 16/50 VUs  0m40.2s/2m50.0s

running (0m44.0s), 17/50 VUs, 796 complete and 0 interrupted iterations
order_submission_baseline   [  24% ] 17/50 VUs  0m41.2s/2m50.0s

running (0m45.0s), 18/50 VUs, 843 complete and 0 interrupted iterations
order_submission_baseline   [  25% ] 18/50 VUs  0m42.2s/2m50.0s

running (0m46.0s), 18/50 VUs, 888 complete and 0 interrupted iterations
order_submission_baseline   [  25% ] 18/50 VUs  0m43.2s/2m50.0s

running (0m47.0s), 19/50 VUs, 944 complete and 0 interrupted iterations
order_submission_baseline   [  26% ] 19/50 VUs  0m44.2s/2m50.0s

running (0m48.0s), 20/50 VUs, 998 complete and 0 interrupted iterations
order_submission_baseline   [  27% ] 20/50 VUs  0m45.2s/2m50.0s

running (0m49.0s), 20/50 VUs, 1059 complete and 0 interrupted iterations
order_submission_baseline   [  27% ] 20/50 VUs  0m46.2s/2m50.0s

running (0m50.0s), 21/50 VUs, 1116 complete and 0 interrupted iterations
order_submission_baseline   [  28% ] 21/50 VUs  0m47.2s/2m50.0s

running (0m51.0s), 22/50 VUs, 1171 complete and 0 interrupted iterations
order_submission_baseline   [  28% ] 22/50 VUs  0m48.2s/2m50.0s

running (0m52.0s), 22/50 VUs, 1233 complete and 0 interrupted iterations
order_submission_baseline   [  29% ] 22/50 VUs  0m49.2s/2m50.0s

running (0m53.0s), 23/50 VUs, 1300 complete and 0 interrupted iterations
order_submission_baseline   [  30% ] 23/50 VUs  0m50.2s/2m50.0s

running (0m54.0s), 24/50 VUs, 1371 complete and 0 interrupted iterations
order_submission_baseline   [  30% ] 24/50 VUs  0m51.2s/2m50.0s

running (0m55.0s), 24/50 VUs, 1432 complete and 0 interrupted iterations
order_submission_baseline   [  31% ] 24/50 VUs  0m52.2s/2m50.0s

running (0m56.0s), 25/50 VUs, 1497 complete and 0 interrupted iterations
order_submission_baseline   [  31% ] 25/50 VUs  0m53.2s/2m50.0s

running (0m57.0s), 26/50 VUs, 1568 complete and 0 interrupted iterations
order_submission_baseline   [  32% ] 26/50 VUs  0m54.2s/2m50.0s

running (0m58.0s), 26/50 VUs, 1639 complete and 0 interrupted iterations
order_submission_baseline   [  32% ] 26/50 VUs  0m55.2s/2m50.0s

running (0m59.0s), 27/50 VUs, 1714 complete and 0 interrupted iterations
order_submission_baseline   [  33% ] 27/50 VUs  0m56.2s/2m50.0s

running (1m00.0s), 28/50 VUs, 1784 complete and 0 interrupted iterations
order_submission_baseline   [  34% ] 28/50 VUs  0m57.2s/2m50.0s

running (1m01.0s), 28/50 VUs, 1865 complete and 0 interrupted iterations
order_submission_baseline   [  34% ] 28/50 VUs  0m58.2s/2m50.0s

running (1m02.0s), 29/50 VUs, 1944 complete and 0 interrupted iterations
order_submission_baseline   [  35% ] 29/50 VUs  0m59.2s/2m50.0s

running (1m03.0s), 30/50 VUs, 2026 complete and 0 interrupted iterations
order_submission_baseline   [  35% ] 30/50 VUs  1m00.2s/2m50.0s

running (1m04.0s), 30/50 VUs, 2111 complete and 0 interrupted iterations
order_submission_baseline   [  36% ] 30/50 VUs  1m01.2s/2m50.0s

running (1m05.0s), 31/50 VUs, 2195 complete and 0 interrupted iterations
order_submission_baseline   [  37% ] 31/50 VUs  1m02.2s/2m50.0s

running (1m06.0s), 32/50 VUs, 2278 complete and 0 interrupted iterations
order_submission_baseline   [  37% ] 32/50 VUs  1m03.2s/2m50.0s

running (1m07.0s), 32/50 VUs, 2371 complete and 0 interrupted iterations
order_submission_baseline   [  38% ] 32/50 VUs  1m04.2s/2m50.0s

running (1m08.0s), 33/50 VUs, 2462 complete and 0 interrupted iterations
order_submission_baseline   [  38% ] 33/50 VUs  1m05.2s/2m50.0s

running (1m09.0s), 34/50 VUs, 2550 complete and 0 interrupted iterations
order_submission_baseline   [  39% ] 34/50 VUs  1m06.2s/2m50.0s

running (1m10.0s), 34/50 VUs, 2643 complete and 0 interrupted iterations
order_submission_baseline   [  40% ] 34/50 VUs  1m07.2s/2m50.0s

running (1m11.0s), 35/50 VUs, 2736 complete and 0 interrupted iterations
order_submission_baseline   [  40% ] 35/50 VUs  1m08.2s/2m50.0s

running (1m12.0s), 36/50 VUs, 2839 complete and 0 interrupted iterations
order_submission_baseline   [  41% ] 36/50 VUs  1m09.2s/2m50.0s

running (1m13.0s), 36/50 VUs, 2934 complete and 0 interrupted iterations
order_submission_baseline   [  41% ] 36/50 VUs  1m10.2s/2m50.0s

running (1m14.0s), 37/50 VUs, 3032 complete and 0 interrupted iterations
order_submission_baseline   [  42% ] 37/50 VUs  1m11.2s/2m50.0s

running (1m15.0s), 38/50 VUs, 3133 complete and 0 interrupted iterations
order_submission_baseline   [  42% ] 38/50 VUs  1m12.2s/2m50.0s

running (1m16.0s), 38/50 VUs, 3241 complete and 0 interrupted iterations
order_submission_baseline   [  43% ] 38/50 VUs  1m13.2s/2m50.0s

running (1m17.0s), 39/50 VUs, 3345 complete and 0 interrupted iterations
order_submission_baseline   [  44% ] 39/50 VUs  1m14.2s/2m50.0s

running (1m18.0s), 40/50 VUs, 3450 complete and 0 interrupted iterations
order_submission_baseline   [  44% ] 40/50 VUs  1m15.2s/2m50.0s

running (1m19.0s), 40/50 VUs, 3567 complete and 0 interrupted iterations
order_submission_baseline   [  45% ] 40/50 VUs  1m16.2s/2m50.0s

running (1m20.0s), 41/50 VUs, 3685 complete and 0 interrupted iterations
order_submission_baseline   [  45% ] 41/50 VUs  1m17.2s/2m50.0s

running (1m21.0s), 42/50 VUs, 3797 complete and 0 interrupted iterations
order_submission_baseline   [  46% ] 42/50 VUs  1m18.2s/2m50.0s

running (1m22.0s), 42/50 VUs, 3916 complete and 0 interrupted iterations
order_submission_baseline   [  47% ] 42/50 VUs  1m19.2s/2m50.0s

running (1m23.0s), 43/50 VUs, 4037 complete and 0 interrupted iterations
order_submission_baseline   [  47% ] 43/50 VUs  1m20.2s/2m50.0s

running (1m24.0s), 44/50 VUs, 4153 complete and 0 interrupted iterations
order_submission_baseline   [  48% ] 44/50 VUs  1m21.2s/2m50.0s

running (1m25.0s), 44/50 VUs, 4275 complete and 0 interrupted iterations
order_submission_baseline   [  48% ] 44/50 VUs  1m22.2s/2m50.0s

running (1m26.0s), 45/50 VUs, 4400 complete and 0 interrupted iterations
order_submission_baseline   [  49% ] 45/50 VUs  1m23.2s/2m50.0s

running (1m27.0s), 46/50 VUs, 4523 complete and 0 interrupted iterations
order_submission_baseline   [  50% ] 46/50 VUs  1m24.2s/2m50.0s

running (1m28.0s), 46/50 VUs, 4643 complete and 0 interrupted iterations
order_submission_baseline   [  50% ] 46/50 VUs  1m25.2s/2m50.0s

running (1m29.0s), 47/50 VUs, 4773 complete and 0 interrupted iterations
order_submission_baseline   [  51% ] 47/50 VUs  1m26.2s/2m50.0s

running (1m30.0s), 48/50 VUs, 4905 complete and 0 interrupted iterations
order_submission_baseline   [  51% ] 48/50 VUs  1m27.2s/2m50.0s

running (1m31.0s), 48/50 VUs, 5038 complete and 0 interrupted iterations
order_submission_baseline   [  52% ] 48/50 VUs  1m28.2s/2m50.0s

running (1m32.0s), 49/50 VUs, 5173 complete and 0 interrupted iterations
order_submission_baseline   [  52% ] 49/50 VUs  1m29.2s/2m50.0s

running (1m33.0s), 50/50 VUs, 5310 complete and 0 interrupted iterations
order_submission_baseline   [  53% ] 50/50 VUs  1m30.2s/2m50.0s

running (1m34.0s), 50/50 VUs, 5450 complete and 0 interrupted iterations
order_submission_baseline   [  54% ] 50/50 VUs  1m31.2s/2m50.0s

running (1m35.0s), 50/50 VUs, 5592 complete and 0 interrupted iterations
order_submission_baseline   [  54% ] 50/50 VUs  1m32.2s/2m50.0s

running (1m36.0s), 50/50 VUs, 5736 complete and 0 interrupted iterations
order_submission_baseline   [  55% ] 50/50 VUs  1m33.2s/2m50.0s

running (1m37.0s), 50/50 VUs, 5872 complete and 0 interrupted iterations
order_submission_baseline   [  55% ] 50/50 VUs  1m34.2s/2m50.0s

running (1m38.0s), 50/50 VUs, 6015 complete and 0 interrupted iterations
order_submission_baseline   [  56% ] 50/50 VUs  1m35.2s/2m50.0s

running (1m39.0s), 50/50 VUs, 6155 complete and 0 interrupted iterations
order_submission_baseline   [  57% ] 50/50 VUs  1m36.2s/2m50.0s

running (1m40.0s), 50/50 VUs, 6295 complete and 0 interrupted iterations
order_submission_baseline   [  57% ] 50/50 VUs  1m37.2s/2m50.0s

running (1m41.0s), 50/50 VUs, 6428 complete and 0 interrupted iterations
order_submission_baseline   [  58% ] 50/50 VUs  1m38.2s/2m50.0s

running (1m42.0s), 50/50 VUs, 6568 complete and 0 interrupted iterations
order_submission_baseline   [  58% ] 50/50 VUs  1m39.2s/2m50.0s

running (1m43.0s), 50/50 VUs, 6708 complete and 0 interrupted iterations
order_submission_baseline   [  59% ] 50/50 VUs  1m40.2s/2m50.0s

running (1m44.0s), 50/50 VUs, 6844 complete and 0 interrupted iterations
order_submission_baseline   [  60% ] 50/50 VUs  1m41.2s/2m50.0s

running (1m45.0s), 50/50 VUs, 6979 complete and 0 interrupted iterations
order_submission_baseline   [  60% ] 50/50 VUs  1m42.2s/2m50.0s

running (1m46.0s), 50/50 VUs, 7120 complete and 0 interrupted iterations
order_submission_baseline   [  61% ] 50/50 VUs  1m43.2s/2m50.0s

running (1m47.0s), 50/50 VUs, 7266 complete and 0 interrupted iterations
order_submission_baseline   [  61% ] 50/50 VUs  1m44.2s/2m50.0s

running (1m48.0s), 50/50 VUs, 7405 complete and 0 interrupted iterations
order_submission_baseline   [  62% ] 50/50 VUs  1m45.2s/2m50.0s

running (1m49.0s), 50/50 VUs, 7544 complete and 0 interrupted iterations
order_submission_baseline   [  62% ] 50/50 VUs  1m46.2s/2m50.0s

running (1m50.0s), 50/50 VUs, 7679 complete and 0 interrupted iterations
order_submission_baseline   [  63% ] 50/50 VUs  1m47.2s/2m50.0s

running (1m51.0s), 50/50 VUs, 7815 complete and 0 interrupted iterations
order_submission_baseline   [  64% ] 50/50 VUs  1m48.2s/2m50.0s

running (1m52.0s), 50/50 VUs, 7947 complete and 0 interrupted iterations
order_submission_baseline   [  64% ] 50/50 VUs  1m49.2s/2m50.0s

running (1m53.0s), 50/50 VUs, 8083 complete and 0 interrupted iterations
order_submission_baseline   [  65% ] 50/50 VUs  1m50.2s/2m50.0s

running (1m54.0s), 50/50 VUs, 8216 complete and 0 interrupted iterations
order_submission_baseline   [  65% ] 50/50 VUs  1m51.2s/2m50.0s

running (1m55.0s), 50/50 VUs, 8358 complete and 0 interrupted iterations
order_submission_baseline   [  66% ] 50/50 VUs  1m52.2s/2m50.0s

running (1m56.0s), 50/50 VUs, 8499 complete and 0 interrupted iterations
order_submission_baseline   [  67% ] 50/50 VUs  1m53.2s/2m50.0s

running (1m57.0s), 50/50 VUs, 8633 complete and 0 interrupted iterations
order_submission_baseline   [  67% ] 50/50 VUs  1m54.2s/2m50.0s

running (1m58.0s), 50/50 VUs, 8775 complete and 0 interrupted iterations
order_submission_baseline   [  68% ] 50/50 VUs  1m55.2s/2m50.0s

running (1m59.0s), 50/50 VUs, 8919 complete and 0 interrupted iterations
order_submission_baseline   [  68% ] 50/50 VUs  1m56.2s/2m50.0s

running (2m00.0s), 50/50 VUs, 9055 complete and 0 interrupted iterations
order_submission_baseline   [  69% ] 50/50 VUs  1m57.2s/2m50.0s

running (2m01.0s), 50/50 VUs, 9199 complete and 0 interrupted iterations
order_submission_baseline   [  70% ] 50/50 VUs  1m58.2s/2m50.0s

running (2m02.0s), 50/50 VUs, 9332 complete and 0 interrupted iterations
order_submission_baseline   [  70% ] 50/50 VUs  1m59.2s/2m50.0s

running (2m03.0s), 50/50 VUs, 9477 complete and 0 interrupted iterations
order_submission_baseline   [  71% ] 50/50 VUs  2m00.2s/2m50.0s

running (2m04.0s), 50/50 VUs, 9611 complete and 0 interrupted iterations
order_submission_baseline   [  71% ] 50/50 VUs  2m01.2s/2m50.0s

running (2m05.0s), 50/50 VUs, 9752 complete and 0 interrupted iterations
order_submission_baseline   [  72% ] 50/50 VUs  2m02.2s/2m50.0s

running (2m06.0s), 50/50 VUs, 9899 complete and 0 interrupted iterations
order_submission_baseline   [  72% ] 50/50 VUs  2m03.2s/2m50.0s

running (2m07.0s), 50/50 VUs, 10044 complete and 0 interrupted iterations
order_submission_baseline   [  73% ] 50/50 VUs  2m04.2s/2m50.0s

running (2m08.0s), 50/50 VUs, 10186 complete and 0 interrupted iterations
order_submission_baseline   [  74% ] 50/50 VUs  2m05.2s/2m50.0s

running (2m09.0s), 50/50 VUs, 10321 complete and 0 interrupted iterations
order_submission_baseline   [  74% ] 50/50 VUs  2m06.2s/2m50.0s

running (2m10.0s), 50/50 VUs, 10466 complete and 0 interrupted iterations
order_submission_baseline   [  75% ] 50/50 VUs  2m07.2s/2m50.0s

running (2m11.0s), 50/50 VUs, 10613 complete and 0 interrupted iterations
order_submission_baseline   [  75% ] 50/50 VUs  2m08.2s/2m50.0s

running (2m12.0s), 50/50 VUs, 10754 complete and 0 interrupted iterations
order_submission_baseline   [  76% ] 50/50 VUs  2m09.2s/2m50.0s

running (2m13.0s), 50/50 VUs, 10891 complete and 0 interrupted iterations
order_submission_baseline   [  77% ] 50/50 VUs  2m10.2s/2m50.0s

running (2m14.0s), 50/50 VUs, 11034 complete and 0 interrupted iterations
order_submission_baseline   [  77% ] 50/50 VUs  2m11.2s/2m50.0s

running (2m15.0s), 50/50 VUs, 11167 complete and 0 interrupted iterations
order_submission_baseline   [  78% ] 50/50 VUs  2m12.2s/2m50.0s

running (2m16.0s), 50/50 VUs, 11308 complete and 0 interrupted iterations
order_submission_baseline   [  78% ] 50/50 VUs  2m13.2s/2m50.0s

running (2m17.0s), 50/50 VUs, 11441 complete and 0 interrupted iterations
order_submission_baseline   [  79% ] 50/50 VUs  2m14.2s/2m50.0s

running (2m18.0s), 50/50 VUs, 11576 complete and 0 interrupted iterations
order_submission_baseline   [  80% ] 50/50 VUs  2m15.2s/2m50.0s

running (2m19.0s), 50/50 VUs, 11715 complete and 0 interrupted iterations
order_submission_baseline   [  80% ] 50/50 VUs  2m16.2s/2m50.0s

running (2m20.0s), 50/50 VUs, 11853 complete and 0 interrupted iterations
order_submission_baseline   [  81% ] 50/50 VUs  2m17.2s/2m50.0s

running (2m21.0s), 50/50 VUs, 11985 complete and 0 interrupted iterations
order_submission_baseline   [  81% ] 50/50 VUs  2m18.2s/2m50.0s

running (2m22.0s), 50/50 VUs, 12132 complete and 0 interrupted iterations
order_submission_baseline   [  82% ] 50/50 VUs  2m19.2s/2m50.0s

running (2m23.0s), 50/50 VUs, 12265 complete and 0 interrupted iterations
order_submission_baseline   [  82% ] 50/50 VUs  2m20.2s/2m50.0s

running (2m24.0s), 50/50 VUs, 12403 complete and 0 interrupted iterations
order_submission_baseline   [  83% ] 50/50 VUs  2m21.2s/2m50.0s

running (2m25.0s), 50/50 VUs, 12532 complete and 0 interrupted iterations
order_submission_baseline   [  84% ] 50/50 VUs  2m22.2s/2m50.0s

running (2m26.0s), 50/50 VUs, 12668 complete and 0 interrupted iterations
order_submission_baseline   [  84% ] 50/50 VUs  2m23.2s/2m50.0s

running (2m27.0s), 50/50 VUs, 12805 complete and 0 interrupted iterations
order_submission_baseline   [  85% ] 50/50 VUs  2m24.2s/2m50.0s

running (2m28.0s), 50/50 VUs, 12942 complete and 0 interrupted iterations
order_submission_baseline   [  85% ] 50/50 VUs  2m25.2s/2m50.0s

running (2m29.0s), 50/50 VUs, 13076 complete and 0 interrupted iterations
order_submission_baseline   [  86% ] 50/50 VUs  2m26.2s/2m50.0s

running (2m30.0s), 50/50 VUs, 13216 complete and 0 interrupted iterations
order_submission_baseline   [  87% ] 50/50 VUs  2m27.2s/2m50.0s

running (2m31.0s), 50/50 VUs, 13354 complete and 0 interrupted iterations
order_submission_baseline   [  87% ] 50/50 VUs  2m28.2s/2m50.0s

running (2m32.0s), 50/50 VUs, 13488 complete and 0 interrupted iterations
order_submission_baseline   [  88% ] 50/50 VUs  2m29.2s/2m50.0s

running (2m33.0s), 50/50 VUs, 13629 complete and 0 interrupted iterations
order_submission_baseline   [  88% ] 50/50 VUs  2m30.2s/2m50.0s

running (2m34.0s), 48/50 VUs, 13764 complete and 0 interrupted iterations
order_submission_baseline   [  89% ] 48/50 VUs  2m31.2s/2m50.0s

running (2m35.0s), 46/50 VUs, 13895 complete and 0 interrupted iterations
order_submission_baseline   [  90% ] 46/50 VUs  2m32.2s/2m50.0s

running (2m36.0s), 44/50 VUs, 14016 complete and 0 interrupted iterations
order_submission_baseline   [  90% ] 44/50 VUs  2m33.2s/2m50.0s

running (2m37.0s), 41/50 VUs, 14130 complete and 0 interrupted iterations
order_submission_baseline   [  91% ] 41/50 VUs  2m34.2s/2m50.0s

running (2m38.0s), 39/50 VUs, 14240 complete and 0 interrupted iterations
order_submission_baseline   [  91% ] 39/50 VUs  2m35.2s/2m50.0s

running (2m39.0s), 35/50 VUs, 14347 complete and 0 interrupted iterations
order_submission_baseline   [  92% ] 35/50 VUs  2m36.2s/2m50.0s

running (2m40.0s), 33/50 VUs, 14444 complete and 0 interrupted iterations
order_submission_baseline   [  92% ] 33/50 VUs  2m37.2s/2m50.0s

running (2m41.0s), 31/50 VUs, 14533 complete and 0 interrupted iterations
order_submission_baseline   [  93% ] 31/50 VUs  2m38.2s/2m50.0s

running (2m42.0s), 28/50 VUs, 14620 complete and 0 interrupted iterations
order_submission_baseline   [  94% ] 28/50 VUs  2m39.2s/2m50.0s

running (2m43.0s), 25/50 VUs, 14695 complete and 0 interrupted iterations
order_submission_baseline   [  94% ] 25/50 VUs  2m40.2s/2m50.0s

running (2m44.0s), 23/50 VUs, 14766 complete and 0 interrupted iterations
order_submission_baseline   [  95% ] 23/50 VUs  2m41.2s/2m50.0s

running (2m45.0s), 21/50 VUs, 14821 complete and 0 interrupted iterations
order_submission_baseline   [  95% ] 21/50 VUs  2m42.2s/2m50.0s

running (2m46.0s), 18/50 VUs, 14884 complete and 0 interrupted iterations
order_submission_baseline   [  96% ] 18/50 VUs  2m43.2s/2m50.0s

running (2m47.0s), 16/50 VUs, 14931 complete and 0 interrupted iterations
order_submission_baseline   [  97% ] 16/50 VUs  2m44.2s/2m50.0s

running (2m48.0s), 13/50 VUs, 14972 complete and 0 interrupted iterations
order_submission_baseline   [  97% ] 13/50 VUs  2m45.2s/2m50.0s

running (2m49.0s), 11/50 VUs, 15009 complete and 0 interrupted iterations
order_submission_baseline   [  98% ] 11/50 VUs  2m46.2s/2m50.0s

running (2m50.0s), 08/50 VUs, 15034 complete and 0 interrupted iterations
order_submission_baseline   [  98% ] 08/50 VUs  2m47.2s/2m50.0s

running (2m51.0s), 05/50 VUs, 15055 complete and 0 interrupted iterations
order_submission_baseline   [  99% ] 05/50 VUs  2m48.2s/2m50.0s

running (2m52.0s), 03/50 VUs, 15068 complete and 0 interrupted iterations
order_submission_baseline   [ 100% ] 03/50 VUs  2m49.2s/2m50.0s

running (2m53.0s), 01/50 VUs, 15074 complete and 0 interrupted iterations
order_submission_baseline ↓ [ 100% ] 01/50 VUs  2m50s


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
