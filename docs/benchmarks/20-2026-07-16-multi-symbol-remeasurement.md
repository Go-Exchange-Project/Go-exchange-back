# 20번째 테스트 (2026-07-16): 다중 심볼 부하 재측정 — B-1b/B-1c 실측 + 엔진 병목 판정

## 커밋 해시

- **A (before)**: `f3103c0` — B-1c 직전(19번 벤치마크의 B와 동일 코드)
- **B (after)**: `e98f4ee` — 현 main. B-1c(체결 브로드캐스트 배치화, `83d3f4c`+`f018042`) 포함. 배포 트리에는 이번 세션의 loadtest 스크립트 커밋(`e98f4ee`)도 포함되지만 백엔드 코드(`cmd/`, `internal/`)는 `f018042`와 동일하다.

## 왜 이 테스트를 했는지

[19번 벤치마크](19-2026-07-16-b1-b4-batch-remeasurement.md)는 세 가지 잔여 항목을 남겼다:

1. **B-1c 실측 불가** — 19번 당시 B-1c가 아직 없었다(19번 해석 2가 발견한 병목의 후속 조치로 B-1c가 나왔다).
2. **B-1b 실측 불가** — 19번의 부하 프로파일이 BTC 단일 심볼만 거래해, legacy와 subscribe가 받는 메시지가 사실상 동일했다.
3. **엔진 병목 미판정** — B-3(매칭 엔진 심볼 샤딩) 착수 여부를 실측 없이 결정할 수 없었다(16번 벤치마크의 교훈: 병목이 아닌 곳을 먼저 최적화하지 않는다).

세 가지 모두 **다중 심볼 부하 프로파일**이 있어야 실측 가능하다는 것이 공통 결론이었다. 이번 테스트는 8개 심볼로 확장한 새 부하 스크립트로 세 가지를 한 세션에서 검증한다.

## 왜 이 방식을 선택했는지 (방법론 — 17·18·19번 계승)

- **같은 VM·같은 세션 연속 A/B**: 서버·DB VM은 이번 세션 동안 stop/start하지 않았다(18번에서 실증된 세션 교란 회피). A(`f3103c0`)로 측정 1·2 → B(main)로 재배포 후 측정 3·4·5, 전부 한 세션. load-gen만 19번 리사이즈 상태(`e2-standard-8`)를 그대로 재사용했다(측정 대상이 아니므로 세션 규칙과 무관).
- **19번 수치와 직접 비교하지 않는다** — 프로파일(단일 vs 8심볼)도 세션도 다르다. 이번 세션의 측정 1·3이 유일한 기준선이다.
- **다중 심볼 부하 스크립트(신규, 리포에 커밋)**: `loadtest/order-submission-multisymbol.js` — `SYMBOLS = ['BTC','ETH','XRP','SOL','ADA','DOGE','TRX','DOT']`, 유저 `i`의 심볼을 `floor((i-1)/2) % 8`로 배정해 연속된 두 유저(매수/매도 쌍)가 항상 같은 심볼을 갖게 했다. **XRP 함정**: `config/market_rules.json`에 XRP가 정수 단위(`min_order_quantity=1`, `base_quantity_step=1`)로 등록돼 있어, 다른 심볼과 같은 소수점 수량(`0.001`)을 쓰면 주문이 거부된다 — XRP만 주문 수량 `1`로 분리하고, 그만큼 명목가가 커지는 것을 감안해 지갑 충전액을 넉넉히(코인 100만 단위, KRW 1조) 잡았다. 로컬 스모크(유저 16명, 1분)로 8개 심볼 전부에서 체결·정산(`failed_settlements=0`)을 확인한 뒤 GCP로 진행했다.
- **WS 부하 스크립트**(`loadtest/ws-load.js`, 19번 문서 첨부본을 리포로 승격): `SUBSCRIBE_SYMBOLS` 환경변수로 구독 심볼을 지정할 수 있게 확장(측정 5는 `BTC` 1개만 구독 — 8개 중 1개이므로 이번에는 legacy 대비 실제 차이가 나야 정상).
- **매 측정 전 리셋**: TRUNCATE(19번 문서와 동일 9개 테이블) + backend 재시작 + `matching bootstrap completed: loaded=0` 확인. 5회 전부 수행.
- **pprof**: `GOEXCHANGE_ENABLE_PPROF=true`가 이전 세션부터 두 배포(`bench-b19`/`bench-b20`)의 `.env`에 이미 설정돼 있어 A/B 동일 조건이 자동으로 유지됐다. 측정 3(B, REST 단독) hold 중반에 `gcloud compute ssh --ssh-flag="-L 6060:localhost:6060"`로 터널을 열고 `go tool pprof -seconds=30 http://localhost:6060/debug/pprof/profile`로 30초 CPU 프로파일을 캡처했다(4번 벤치마크 절차 계승).
- **엔진 채널 게이지**: `/metrics`의 `matching_engine_channel_length`·`settlement_worker_queue_length`를 측정 3 동안 30초 간격 20회 스크레이프했다(서버 `/metrics`가 load-gen과 관리자 IP에 공개돼 있어 로컬에서 직접 curl).

## 실행한 정확한 커맨드

리셋(db VM, 컨테이너 내부) — 19번과 동일:

```bash
docker exec goexchange-db-postgres bash -c \
  'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "TRUNCATE TABLE failed_market_completions, failed_settlements, ledger_entries, orders, reconciliation_violations, trade_outbox_events, trades, users, wallets RESTART IDENTITY CASCADE;"'
```

배포(서버 VM):

```bash
docker compose -f docker-compose.stress.yml up -d --build backend   # A: bench-b19(f3103c0), B: bench-b20(main)
docker compose -f docker-compose.stress.yml restart backend          # 리셋마다
```

REST hold(load-gen VM, 다중 심볼):

```bash
k6 run -e BASE_URL=http://10.10.0.3:8080 -e DEV_TOOLS_TOKEN=$(cat ~/.devtoolstoken) ~/order-submission-multisymbol.js
```

WS 부하(REST와 동시, 별도 프로세스):

```bash
# legacy(측정 2, 4)
k6 run -e BASE_URL=http://10.10.0.3:8080 -e MODE=legacy -e N=300 -e DURATION=9m40s -e HOLD_MS=570000 ~/ws-load.js
# subscribe(측정 5, BTC 1심볼)
k6 run -e BASE_URL=http://10.10.0.3:8080 -e MODE=subscribe -e SUBSCRIBE_SYMBOLS=BTC -e N=300 -e DURATION=9m40s -e HOLD_MS=570000 ~/ws-load.js
```

채널 게이지 샘플링(로컬, 서버 외부 IP 직접 접근):

```bash
curl -s "http://<server-external-ip>:8080/metrics" | grep -E "matching_engine_channel_length|settlement_worker_queue_length"
# 30초 간격 20회 반복
```

pprof(로컬, IAP 없이 직접 SSH 터널 — 방화벽이 관리자 IP의 22번 포트를 허용):

```bash
gcloud compute ssh goexchange-stress-server --zone=asia-northeast3-a --ssh-flag="-L 6060:localhost:6060" --command="sleep 45" &
go tool pprof -seconds=30 http://localhost:6060/debug/pprof/profile
```

환경: 서버 `e2-highcpu-4`(4 vCPU), DB `e2-highcpu-4`, load-gen `e2-standard-8`(19번에서 리사이즈된 상태 유지), k6 v2.0.0, 서울 리전.

## 핵심 결과

### REST 처리량 (측정 1·2·3·4·5)

| # | 빌드 | WS | iterations | 처리량(iter/s) | http_req p95 | http_req_failed |
|---|---|---|---|---|---|---|
| 1 | A | 없음 | 963,003 | **1499.07** | 1.82s | 0.00% |
| 2 | A | legacy×300 | 467,442 | **727.32** (−51.5% vs 1) | 4.21s | 0.00% |
| 3 | B | 없음 | 950,226 | **1478.98** (−1.3% vs 1, 거의 중립) | 1.83s | 0.00% |
| 4 | B | legacy×300 | 774,895 | **1203.68** (−18.6% vs 3) | 2.20s | 0.00% |
| 5 | B | subscribe×300(BTC) | 903,024 | **1405.49** (−4.9% vs 3) | 1.89s | 0.00% |

**판정(B-1c)**: 4/3 비율(0.8138) > 2/1 비율(0.4852) — **뚜렷이 충족**. 19번(단일 심볼)에서는 이 기준을 충족하지 못했는데(4/3=0.665 < 2/1=0.761), 다중 심볼 프로파일에서는 명확히 뒤집혔다.

### WS 클라이언트 수신 메시지 및 연결 안정성 (측정 2·4·5, 300 클라이언트·9.5분)

| # | 빌드 | 모드 | 총 수신 메시지 | 연결 총횟수 | 평균 세션 유지시간 |
|---|---|---|---|---|---|
| 2 | A | legacy | 65,507,763 | **1,389** | 2m13s |
| 4 | B | legacy | 16,725,646 (−74.5%, 약 3.9:1) | **600** | 9m30s |
| 5 | B | subscribe(BTC) | 2,265,986 (−86.5% vs 4, 약 7.4:1) | 600 | 9m30s |

`HOLD_MS`(570s) < 시나리오 `DURATION`(580s)이라 정상적으로도 VU당 2회 연결(합 600)이 발생한다 — **측정 4·5는 이 기준선과 정확히 일치**(강제 종료 0건). 반면 **측정 2(A)는 1,389회로 기준선의 2배 이상** — hub의 `client.Send` 채널(버퍼 256)이 가득 차면 non-blocking select의 `default` 분기가 즉시 `unregisterClient`로 연결을 끊는 코드(`internal/ws/hub.go:100-110`) 때문으로 보인다. A는 8개 심볼의 체결마다 개별 메시지를 브로드캐스트해(B-1c 이전) 클라이언트가 못 따라가면 통째로 끊기고 k6가 즉시 재연결하는 패턴이 반복된 것이다. **판정(B-1b)**: 측정 5의 수신 메시지가 측정 4의 1/4 미만(기준) — 실제로는 1/7.4로 이론치(8개 중 1개 구독 → 1/8)에 근접, **명확히 충족**.

### settlement_batch_size 스냅샷 (측정 3·4·5 직후 `/metrics`)

| # | count | sum | avg size | fallbacks |
|---|---|---|---|---|
| 3 | 24,251 | 474,838 | 19.58 | 0 |
| 4 | 17,559 | 387,274 | 22.06 | 0 |
| 5 | 22,580 | 451,372 | 19.99 | 0 |

단일 심볼(19번, 평균 ~31.1·상한 도달률 97%)보다 평균 배치 크기가 작다 — 8개 심볼로 워커가 분산돼(심볼 해시 파티션) 워커당 유입량이 줄었기 때문으로 보인다. `fallbacks_total`은 세 스냅샷 모두 0.

### 엔진 병목 판정 데이터 (측정 3 중 수집)

**pprof CPU 30초 (hold 중반, 4 vCPU 서버, 총 샘플 83.31s/30.16s = 276% 평균 코어 점유)**:

```
      flat  flat%   sum%        cum   cum%
    15.47s 18.57% 18.57%     15.47s 18.57%  internal/runtime/syscall.Syscall6
     2.68s  3.22% 21.79%      3.29s  3.95%  runtime.findObject
     1.99s  2.39% 24.17%      1.99s  2.39%  runtime.nextFreeFast (inline)
     ... (GC/할당 관련 함수들이 상위권 다수)
```

애플리케이션 함수만 추려서(cum 기준):

```
     0.06s 0.072% ...     41.54s 49.86%  internal/handler.(*OrderHandler).CreateOrder
     0.03s 0.036% ...     39.82s 47.80%  internal/service.(*OrderService).CreateOrder
     0.02s 0.024% ...     22.48s 26.98%  internal/service.holdOrderAssets
     0.09s 0.11%  ...     16.71s 20.06%  internal/service.(*SettlementService).SettleTradeBatch.func1
     0.04s 0.048% ...      7.31s  8.77%  internal/repository.(*WalletRepository).updateBalancesDB
     0.02s 0.024% ...      0.43s  0.52%  internal/matching.(*MatchingEngine).Match
```

**매칭 엔진(`Match`)은 CPU의 0.52%(cum, 30초 중 0.43초)에 불과** — 4 vCPU 중 한 코어를 지배하기는커녕 지분이 미미하다. 지배적 비용은 주문 생성 경로(`OrderHandler.CreateOrder` 49.86%, 특히 지갑 락·자금 홀드 `holdOrderAssets` 26.98%)와 정산 배치(`SettleTradeBatch` 20.59%) — 둘 다 DB 왕복이 지배적인 경로다(최상위 flat 항목이 `syscall.Syscall6` 18.57%로, 별도 VM의 DB와의 네트워크 왕복과 정합).

**엔진 채널 게이지 (30초 간격 20회, hold 구간 대부분 커버)**:

```
sample 1 (워밍업 초입): order=0, execution=0
sample 4 (22:34:51)  : order=1024, execution=1024   <- 채널 용량(internal/matching/engine.go:99-102) 상한
sample 5 ~ sample 20  : order=1024 (예외 없음), execution=988~1024 (거의 전 구간 상한 근접)
```

`OrderCh`/`ExecutionCh`의 버퍼 용량은 코드상 1024(`make(chan *Order, 1024)` 등, `internal/matching/engine.go:99-102`)다. **샘플 4부터 20까지(약 8분, 워밍업 종료 직후부터 관측 종료까지 예외 없이) `order` 채널이 정확히 캡 값 1024에 고정**돼 있었다 — 일시적 버스트가 아니라 hold 구간 내내 지속된 적체다. `settlement_worker_queue_length`는 10개 워커 중 5개(1,3,4,6,7)는 항상 0인 반면 2개(worker 8·9)는 주기적으로 200대까지 쌓였다 — 심볼 해시 파티션이 8개 심볼을 10개 슬롯에 고르지 않게 분산시킨 결과로 보인다(부수적 관찰, 이번 판정의 핵심은 아님).

## 판정: 엔진 병목 — **B-3 진행**

계획서의 판정 기준은 "매칭 엔진 goroutine이 ~1 vCPU를 포화 **하거나** 엔진 입력 채널이 지속적으로 적체"다. pprof만 보면 첫 번째 조건은 명백히 미충족(0.52%)이지만, **두 번째 조건(채널의 지속적 적체)은 관측 구간 전체에서 예외 없이 충족**됐다. 두 데이터를 종합하면: 매칭 엔진은 CPU 연산 자체는 저렴하지만, **단일 goroutine이 8개 심볼 전부를 직렬로 처리**하는 구조라 처리율 상한(코어 점유와 무관한 처리량 캡)에 부딪혀 있다 — pprof(CPU 프로파일러)는 "연산이 얼마나 비싼가"만 보여주고 "직렬화로 인한 처리량 캡"은 보여주지 못한다는 것이 이번 조사의 방법론적 교훈이다. `order` 채널이 상시 가득 차 있다는 것은 HTTP 핸들러(주문 제출)들이 엔진에 넣지 못해 대기 중이라는 뜻이고, 이는 측정 3·5의 p95(1.83~1.89s)에도 반영돼 있을 가능성이 높다(엔진 자체 처리는 빠른데 큐잉으로 지연이 붙는 패턴). **B-3(심볼 샤딩)을 진행 확정한다** — 8개 독립 엔진 goroutine으로 나누면 이 직렬화 캡이 최대 8배까지 완화될 것으로 기대되며, 다음 사이클에서 구현 후 이번 측정 3(1478.98 iter/s, 채널 상시 포화)을 기준선으로 재측정한다.

## 원본 출력

### 측정 1 (A, 다중 심볼 REST)

```
checks_total.......: 963003  1499.073686/s
checks_succeeded...: 100.00% 963003 out of 963003
http_req_duration..: avg=1.28s min=3.61ms   med=1.34s max=3.9s  p(90)=1.69s p(95)=1.82s
http_req_failed....: 0.00%  0 out of 969003
http_reqs..........: 969003 1508.413679/s
iterations.........: 963003 1499.073686/s
vus_max............: 3000   min=3000  max=3000
```

### 측정 2 (A + WS legacy N=300)

REST:
```
checks_total.......: 467442  727.321265/s
checks_succeeded...: 100.00% 467442 out of 467442
http_req_duration..: avg=2.99s min=3.85ms   med=3.38s max=5.78s p(90)=4.03s p(95)=4.21s
http_req_failed....: 0.00%  0 out of 473442
http_reqs..........: 473442 736.657028/s
iterations.........: 467442 727.321265/s
```

WS:
```
checks_total...........: 1228    2.013107/s (100% succeeded)
ws_connected_total......: 1389    2.27704/s
ws_messages_received....: 65507763 107389.342862/s
ws_session_duration.....: avg=2m13s min=1.68s med=1m45s max=9m30s p(90)=5m49s p(95)=6m13s
data_received............: 23 GB    37 MB/s
```

### 측정 3 (B, 다중 심볼 REST)

```
checks_total.......: 950226  1478.977101/s
checks_succeeded...: 100.00% 950226 out of 950226
http_req_duration..: avg=1.3s  min=3.61ms   med=1.36s max=4.65s p(90)=1.7s  p(95)=1.83s
http_req_failed....: 0.00%  0 out of 956226
http_reqs..........: 956226 1488.315788/s
iterations.........: 950226 1478.977101/s
vus_max............: 3000   min=3000  max=3000
```

### 측정 4 (B + WS legacy N=300)

REST:
```
checks_total.......: 774895  1203.680039/s
checks_succeeded...: 100.00% 774895 out of 774895
http_req_duration..: avg=1.67s min=4.34ms   med=1.77s max=5.07s p(90)=2.07s p(95)=2.2s
http_req_failed....: 0.00%  0 out of 780895
http_reqs..........: 780895 1213.000115/s
iterations.........: 774895 1203.680039/s
```

WS:
```
checks_total...........: 300     0.491796/s (100% succeeded)
ws_connected_total......: 600     0.983592/s
ws_messages_received....: 16725646 27418.676805/s
ws_session_duration.....: avg=9m30s (전원 만료까지 연결 유지)
data_received............: 38 GB    63 MB/s
```

### 측정 5 (B + WS subscribe N=300, BTC)

REST:
```
checks_total.......: 903024  1405.487262/s
checks_succeeded...: 100.00% 903024 out of 903024
http_req_duration..: avg=1.39s min=3.68ms   med=1.47s max=3.07s p(90)=1.78s p(95)=1.89s
http_req_failed....: 0.00%  0 out of 909024
http_reqs..........: 909024 1414.8258/s
iterations.........: 903024 1405.487262/s
```

WS:
```
checks_total...........: 300     0.4918/s (100% succeeded)
ws_connected_total......: 600     0.983599/s
ws_messages_received....: 2265986 3714.702821/s
ws_session_duration.....: avg=9m30s
data_received............: 5.7 GB  9.3 MB/s
```

## 해석

1. **B-1c 실증 성공.** 다중 심볼 프로파일에서 (측정4÷측정3)=0.814가 (측정2÷측정1)=0.485보다 뚜렷이 높다 — 19번이 충족하지 못한 기준을 이번엔 명확히 충족했다. 절대 처리량도 B+WS(1203.68)가 A+WS(727.32)의 1.65배다. 19번에서는 단일 심볼이라 체결 브로드캐스트 볼륨 자체가 상대적으로 작았지만, 8개 심볼 부하에서는 A(배치화 이전)가 hub 채널 포화로 인한 **강제 연결 종료**(1,389회, 기준선의 2배 이상)까지 겪었다 — B-1c는 처리량뿐 아니라 **연결 안정성**도 개선했다(B는 강제 종료 0건).
2. **B-1b 실증 성공.** 19번은 단일 심볼이라 측정 불가였지만, 이번엔 측정 5(subscribe BTC)가 측정 4(legacy) 대비 메시지를 7.4:1로 줄여(이론치 8:1에 근접) 기준(1/4 미만)을 크게 충족했다.
3. **엔진 병목 판정: B-3 진행.** pprof는 매칭 엔진 자체의 CPU 비용이 미미함을 보였지만(0.52%), `/metrics` 채널 게이지는 `order`/`execution` 채널이 관측 구간 내내(8분, 예외 없이) 정확히 버퍼 상한(1024)에 고정돼 있었음을 보였다. 단일 goroutine이 8개 심볼을 전부 직렬 처리하는 구조가 처리량 캡이 됐다는 뜻이다 — CPU 프로파일만으로는 이 병목을 놓쳤을 것이다(채널 게이지가 없었다면 "엔진은 한가하다"로 오판했을 것). B-3(심볼 샤딩)을 진행 확정한다.
4. **측정 3 vs 1이 거의 중립(−1.3%)** — B-1c는 REST 단독 경로(주문 제출)에 손대지 않으므로 예상대로다. 두 빌드가 사실상 동일한 처리량 캡(엔진 직렬화)에 부딪히고 있다는 방증이기도 하다.
5. **에러율은 5개 측정 전부 0%.** `settlement_batch_fallbacks_total`도 세 스냅샷 모두 0.
6. **다중 심볼 자체의 효과(참고, 19번과 직접 비교는 하지 않음)**: 이번 세션 A(1499 iter/s)가 19번 A(단일 심볼, 156.5 iter/s)보다 절대적으로 훨씬 높다. 심볼별로 독립된 오더북·정산 워커 파티션이 단일 심볼 락 경합을 줄인 결과로 보이나, 세션·프로파일이 달라 정량 비교는 하지 않는다.

## 범위 밖 (Out of Scope)

- **B-3 구현 자체** — 이번 계획은 판정만 한다. 구현·재측정은 다음 사이클.
- 프론트엔드 구독 opt-in 적용 (별도 리포).
- 시장가 경로 벤치마크 (지정가 전용 프로파일 유지).
- `settlement_worker_queue_length`의 워커 간 불균형(worker 8·9만 쌓임) — 부수적 관찰이며 이번 판정에 영향을 주지 않는다. 심볼 해시 파티션 개선 여부는 B-3 설계 시 함께 검토할 만하다.
