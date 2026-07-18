# 22번째 테스트 (2026-07-18): outbox 상한 실증(9번 후속) + 급등락 스파이크 내성

## 커밋 해시

- **배포**: `c899284` — 현 main. A-4(취소-체결 레이스) + 시장가 매수 반올림 오차 수정
  (10번 완료 문서) 포함. 1부는 이 바이너리 하나로 env만 바꾼 same-binary A/B.
  - 측정 1(A): `GOEXCHANGE_OUTBOX_BATCH_SIZE=64` (기동 로그 `outbox writer batch
    size=64` 확인)
  - 측정 2(B): `=512` (기동 로그 `outbox writer batch size=512` 확인)
  - 측정 3(조건부): `=1024` (기동 로그 `outbox writer batch size=1024` 확인)
  - 측정 4(스파이크): `=512`(최적 구성), 샤드 기본값(`shards=1`)

모든 측정에서 `matching engine sharded: shards=1` 기동 로그도 함께 확인했다.

## 왜 이 테스트를 했는지

[9번 완료 문서](../refactor/9_OutboxWriter_처리율_완료.md)는 [21번 벤치마크](21-2026-07-17-b3-sharding-remeasurement.md)가
실측한 outbox flush 포화(배치 상한 64에서 평균 54.4건, ≈827 events/s 캡)를 상한
512로 넓히는 코드 수정만 하고 실측은 이 사이클로 미뤘다. 1부는 그 실증이다 —
같은 바이너리에서 env만 바꾼 A/B(64 vs 512, 조건부 1024)로 ① flush 포화가
실제로 풀리는지 ② execution 채널 만석이 해소되는지 ③ 처리량이 오르는지
④ 다음 병목이 무엇인지를 답한다.

2부는 신규 시나리오다. 17~21번은 전부 지정가 hold(완만한 램프)만 측정했고,
취소·시장가·급격한 부하 변동은 한 번도 실측되지 않았다. 게다가 이 세션
직전에 정확히 그 경로(취소·시장가)에서 정합성 버그 2건(A-4 취소-체결 레이스,
시장가 매수 반올림 오차)이 로컬 스모크로 발견돼 [10번 완료 문서](../refactor/10_A-4_취소체결레이스와_시장가매수반올림오차_완료.md)로
수정됐다. 2부는 그 수정이 GCP 규모(3,000 VU)의 실전 급등락 시나리오에서도
정합성 위반 0으로 버티는지의 최종 실증이다.

## 왜 이 방식을 선택했는지 (방법론 — 17~21번 계승)

- **같은 VM·같은 세션 연속 측정**: 서버·DB VM은 세션 내내 stop/start 없음.
  측정 1(A) → 리셋 → 측정 2(B) → 리셋 → 측정 3(조건부) → 리셋 → 측정 4(스파이크)
  → 드레인 대기 → 정합성 검사 5항목. 매 측정 전 TRUNCATE(9테이블) + backend
  재시작 + `bootstrap loaded=0` 확인.
- **1부는 same-binary A/B**: 배포 1회, env만 바꿔 재시작 — 빌드·배포 변수 0.
- **측정 3 발동 조건**: 측정 2가 측정 1 대비 ±5% 이내(실측 −2.6%)라 조건부 스위프
  1024를 추가해 "더 큰 상한이 도움이 되는가"를 확인했다.
- **1부 수집**: flush 배치 크기/시간 히스토그램(Prometheus 누적 카운터 —
  런 종료 직후, 리셋 전에 읽으면 되므로 중간 타이밍이 필요 없다), execution/order
  채널 게이지 15~30초 간격 샘플, 서버·DB CPU(hold 중반), pprof CPU 30초(측정 2만,
  hold 중반).
- **2부 수집**: 채널 게이지·정산 워커 큐 15초 간격 샘플(스파이크 구간 포함),
  backend 재시작 카운트(`docker inspect RestartCount`), 스테이지별 `POST /orders`
  지연시간(도커 로그 `--since`/`--until` 윈도우로 재구성 — k6 요약은 전체
  집계만 주므로 회복 판정에는 스테이지별 분해가 필요했다).

### 실측 함정: 이 세션에서 겪은 도구 지연 문제

1부 측정 2의 pprof·CPU 캡처를 시도하는 과정에서, 로컬 Bash 도구(특히
`run_in_background` 경유)로 디스패치한 `gcloud compute ssh` 호출이 **9~18분의
불규칙한 지연** 뒤에야 실제로 실행되는 현상을 반복 관측했다(같은 명령을 그대로
재실행해도 재현 — k6 10m43s 실행 시간보다 지연이 길어 4회 연속 "이미 끝난 뒤"
포집하는 실패를 겪었다). 로컬 셸의 순수 `& disown` 백그라운딩(harness의
`run_in_background` 파라미터를 쓰지 않는 방식)으로 전환하자 지연이 사라져
5번째 시도에서 hold 중반 포집에 성공했다. 근본 원인은 특정하지 못했다(세션
내 도구 디스패치 큐 관련으로 추정) — 측정 3·4에서 동일 지연이 재발해 pprof는
측정 2 1회만 확보했고, 측정 4의 스테이지별 CPU 실시간 샘플은 확보하지 못했다
(대신 도커 로그 기반 지연시간 재구성으로 대체). 이 지연은 측정값 자체(k6
처리량·에러율·정합성 검사)에는 영향이 없다 — k6와 정합성 검사는 항상 VM에서
독립적으로 완주했고, 영향받은 것은 로컬에서 원격으로 "언제 스냅샷을 찍는가"뿐이다.

## 실행한 정확한 커맨드

19~21번과 동일한 리셋(`TRUNCATE ... RESTART IDENTITY CASCADE` 9테이블)·배포
(`git archive` → `scp` → `sudo tar` 추출 → 이전 bench 디렉터리 `.env` 복사 →
`sudo docker compose --project-directory /home/goexchange/bench-b22 -f
.../docker-compose.stress.yml up -d --build backend`)·부하(`k6 run -e
BASE_URL=http://10.10.0.3:8080 -e DEV_TOOLS_TOKEN=... ~/order-submission-multisymbol.js`,
8심볼 hold: 1.5분 워밍업 → VU 3000 8분 유지; 측정 4는
`~/order-spike-single-symbol.js`, BTC 단일 심볼 3,000명: 2m@300 → 30초
VU3000 급등 → 3m@3000 유지 → 30초 급락 → 2m@300 회복 관찰).

환경: 서버 `e2-highcpu-4`, DB `e2-highcpu-4`, load-gen `e2-standard-8`,
k6 v2.0.0, 서울 리전.

## 핵심 결과 — 1부 (outbox 상한 실증)

### REST 처리량

| # | env(batch) | iterations | 처리량(iter/s) | p95 | max | http_req_failed |
|---|---|---|---|---|---|---|
| 1(A) | 64 | 933,417 | **1,451.47** | 1.96s | 3.53s | 0.00% |
| 2(B) | 512 | 910,105 | **1,414.32** (−2.6%) | 2.13s | 4.37s | 0.00% |
| 3(조건부) | 1024 | 919,110 | **1,428.47** (−1.6%) | **2.31s** (측정1 대비 +18%) | 4.69s | 0.00% |

### flush 분포 (Prometheus 누적, 런 종료 시점 값)

| # | batch cap | flush 횟수 | 평균 events/flush | 상한 대비 | 평균 flush 시간 |
|---|---|---|---|---|---|
| 2 | 512 | 2,418 | 188.1 | **36.7%** | 59.4ms |
| 3 | 1024 | 2,007 | 228.9 | **22.3%** | 30.4ms |

(21번 참고치: batch=64에서 평균 54.4/64 = **85%**, ≈66ms/flush, 초당 15.2회 —
이번 측정 2·3과 같은 세션이 아니므로 직접 비교하지 않되, 배율 관계는 명확하다.)

`settlement_batch_fallbacks_total`은 측정 1·2·3 전부 0.

### 채널 게이지 (15~30초 간격 샘플, hold 구간)

측정 2·3 모두 **hold 내내 `execution`·`order` 채널이 1024(단일 샤드 상한)에
고정**됐다(워밍업·쿨다운만 0). 측정 3(batch=1024)에서도 동일했다 — 상한을
더 키워도 채널은 풀리지 않았다.

### 자원 (측정 2, hold 중반 실측 — 유일하게 확보한 pprof·CPU 스냅샷)

- 서버 backend: **302.12%/400% CPU**, 호스트 `%Cpu(s): 77.3 us, 18.2 sy, 2.3 id`.
- DB postgres: **315.46%/400% CPU**, 호스트 `%Cpu(s): 59.1 us, 25.0 sy, 4.5 id`
  — 21번(258%, idle 12.8%)보다 더 포화에 가깝다.
- pprof(30.16s, 총 82.01s=272%): `CreateOrder` **50.23%**(cum, 그중
  `holdOrderAssets` 26.67%), `SettleTradeBatch` **19.83%**, 최상위 flat은
  `syscall.Syscall6` 18.86%(DB 네트워크 왕복), `matching.*Match`는 노이즈
  임계값(0.41s) 밑이라 표에 나타나지도 않음 — 21번과 사실상 동일한 서명.

## 1부 판정

**① flush 포화 해제 — 그렇다, 뚜렷하다.** 85%(21번, batch=64) → 36.7%(batch=512)
→ 22.3%(batch=1024). 상한 512에서 이미 여유가 충분하고, 1024는 더 여유롭지만
추가 이득은 없다(아래 ③).

**② execution 채널 만석 해제 — 아니다.** batch를 64→512→1024로 8배·16배
늘려도 채널은 hold 내내 1024에 고정됐다. flush 게이트 자체는 넓어졌지만 그
뒤(DB CPU)가 여전히 좁아 엔진이 여전히 블록된다.

**③ 처리량 상승 폭 — 상승 없음, 오히려 소폭 하락(−2.6%/−1.6%), p95는 뚜렷이
악화(batch=1024에서 +18%).** 수치 예단 없이 기록한다: DB CPU가 idle 4.5%까지
떨어진 상태(측정 2)에서 이미 캡이 걸려 있었다는 뜻이고, flush 게이트를
넓혀도 그 뒤의 실제 처리 능력이 늘지 않으면 처리량은 그대로거나(초과 배칭
오버헤드로) 되레 준다 — Little의 법칙대로 배치가 커질수록 대기 시간(p95)만
늘어난다.

**④ 다음 병목 식별 — DB CPU 확정.** idle 4.5%(측정 2)까지 근접, pprof는
21번과 동일하게 `CreateOrder`(주문 생성 경로, 특히 `holdOrderAssets` 지갑
홀드)와 `SettleTradeBatch`가 지배적 — 21번의 "다음 병목 = DB CPU/주문 생성
DB 왕복" 예측이 그대로 재확인됐다.

**병렬 writer(2단계) 백로그 판정: 기각.** 승격 조건("새 상한에도 flush 포화 +
채널 만석")의 절반만 성립한다 — 채널은 여전히 만석이지만 flush는 더 이상
포화가 아니다(22.7~36.7%). 병목이 write-ahead 게이트에서 DB 자체 용량으로
완전히 넘어갔으므로, 같은 DB에 쓰는 병렬 writer를 추가해도 이 병목을 풀지
못한다 — 기각.

## 핵심 결과 — 2부 (급등락 스파이크 내성)

### 처리량·에러

```
checks_total (주문 생성).......: 100731  181.13/s  (100.00% succeeded, 0% failed)
custom_order_success...........: 100731
custom_market_success..........: 20191
custom_cancel_success...........: 1404
custom_cancel_already_filled....: 8744   (404/409 — 정상, 실패로 안 셈)
custom_cancel_fail..............: 7959   (43.9%, 500 — 아래 원인 분석)
http_req_failed.................: 6.37%  7959 out of 124838 (전량 취소 500과 일치)
http_req_duration (전체 집계)...: avg=5.14s p95=14.83s max=17.64s
backend RestartCount............: 0
```

### 스테이지별 `POST /orders` 지연시간 (도커 로그 `--since`/`--until` 재구성)

| 구간 | 시각(UTC) | n | p50 | p95 | max |
|---|---|---|---|---|---|
| 평시(VU 300) | 20:14:30–20:15:30 | 10,464 | 5.18ms | **7.36ms** | 59.73ms |
| 스파이크 hold(VU 3000) | 20:17:30–20:18:30 | 13,265 | 11.94s | **14.86s** | 14.93s |
| 회복 초반(급락 직후) | 20:20:20–20:20:50 | 6,724 | 4.72ms | **7.74ms** | 995.95ms |
| 회복 후반(관찰 종료 직전) | 20:21:50–20:22:15 | 4,525 | 4.79ms | **7.66ms** | 973.22ms |

### 정합성 검사 5항목 (드레인 확인 — execution 채널·정산 워커 큐 전부 0 —후)

| # | 항목 | 결과 |
|---|---|---|
| 1 | 리컨실리에이션 검사 1(원장-지갑 일치) | **0건** |
| 2 | 리컨실리에이션 검사 2(자산 총량 보존, BTC/KRW) | **diff = 0 / 0** |
| 3 | `failed_settlements` | **0** |
| 4 | `failed_market_completions` | **0** |
| 5-a | 시장가 PENDING/PARTIAL 잔존 | **0**(FILLED 9,943 / CANCELLED 10,248 — 후자는 dust 클램프의 정상 종결 상태, 10번 완료 문서 참고) |
| 5-b | outbox PENDING 잔존 | **0** |
| 5-c | `settlement_batch_fallbacks_total` | **0** |

## 2부 판정

**① 스파이크 전 구간 비-취소 에러율 < 1% — 충족(0%).** 주문 생성
(`checks_total`)은 100,731건 전부 성공. `http_req_failed` 6.37%는 전량 취소
경로에서만 발생(아래).

**② backend 크래시·재시작 0 — 충족.** `RestartCount=0` 확인.

**③ 정합성 검사 5항목 전부 0 — 충족.** 위 표 그대로 전항목 0. **취소-체결
레이스(A-4)와 시장가 매수 반올림 오차(10번 완료 문서) 수정이 GCP 규모
급등락 시나리오에서도 완전히 버틴다는 최종 실증.**

**④ 회복(2분 내 p95 평시 대비 ±50%) — 충족, 사실상 즉시.** 급락 종료
직후(20~50초 이내)부터 이미 p95 7.74ms로 평시(7.36ms) 대비 **+5%**에 불과 —
2분을 기다릴 필요도 없이 회복이 완료됐다.

## 예상 밖 발견: 취소 API 실패율 43.9% — 새로운 실패 모드(A-4 재발 아님)

취소 시도 18,107건(성공 1,404 + 이미체결 8,744 + **실패 7,959**) 중 **43.9%가
진짜 실패(500)**로 나왔다. 도커 로그를 대조한 결과 전부 정확히 `duration=1s`
— `MatchingEngine.CancelOrder`가 `CancelCh`(버퍼 1024)에 커맨드를 보내다
1초 타임아웃(`ErrCancelOrderTimedOut`)에 걸린 것이다(`internal/matching/engine.go`
293-309행의 `select { ...; case <-time.After(time.Second): return
CancelOrderResult{Err: ErrCancelOrderTimedOut} }`). VU 300→3000 급등 구간에서
주문 커맨드가 `CancelCh`를 포화시켜 새 취소 요청이 밀려 들어갈 자리를 못 찾고
1초 뒤 타임아웃되는 것 — **채널 혼잡 실패이지 A-4 레이스의 재발이 아니다**
(정합성 검사 ③이 0을 확인했으므로 자금·상태 정합성은 전혀 깨지지 않았다;
10번 완료 문서의 `isCancelOrderEngineInfraError`가 의도한 대로 이 타임아웃을
409가 아닌 500으로 정확히 분류했다).

이건 이번 스파이크 시나리오가 처음으로 노출한, A-4와는 **독립적인** 새 발견이다
— 사전에 없던 위반이 나오면 그대로 기록하라는 관례에 따라 남긴다.

## 해석

1. **1부: OutboxWriter 처리율 개선(9번)은 "게이트를 넓혔다"는 목표를
   달성했지만, 처리량 개선으로 이어지지는 않았다** — 병목이 이미 그 뒷단
   (DB CPU)으로 넘어가 있었기 때문. 9번 자체가 틀린 건 아니다 — flush
   포화(85%→22~37%)는 실제로 풀렸고, 그 덕에 이번에 DB CPU가 진짜 캡이라는
   것을 정확히 볼 수 있게 됐다(21번 당시엔 flush 포화가 DB 병목을 가리고
   있었을 가능성이 있다). 병렬 writer는 이 병목엔 안 맞는 해법이라 기각한다.
2. **2부: A-4·시장가 버그 수정은 GCP 규모 급등락에서 완전히 버틴다.**
   정합성 검사 5항목 전부 0은 이 프로젝트의 정합성 보증 체인(A-3 write-ahead
   → A-4 취소 시퀀서화 → A-6 리컨실리에이션)이 실전 최악 시나리오에서도
   뚫리지 않는다는 가장 강한 증거다.
3. **새 발견(취소 CancelCh 타임아웃)은 A-4와 무관한 용량 문제다** — 정합성이
   아니라 가용성(availability) 이슈. 아래 후속 항목에 백로그로 남긴다.
4. **도구 디스패치 지연**(이 문서 "실측 함정" 절)으로 측정 2 외에는 hold
   중반 pprof/CPU를 확보하지 못했다 — 측정값의 신뢰도엔 영향 없으나 다음
   세션은 이 문제를 먼저 재현/회피 전략을 검증하고 시작하는 편이 좋다.

## 후속 항목 (로드맵 백로그 후보)

1. **CancelCh 용량/취소 우선순위** (신규, 승격 검토 권고): 극단 스파이크에서
   취소 커맨드가 1초 타임아웃으로 43.9% 유실. 후보: `CancelCh` 버퍼 확대,
   또는 취소를 별도 우선순위 경로로 분리. 정합성엔 무해(409/500 구분이
   정확히 동작)하지만 가용성 개선 여지.
2. **주문 생성 경로 DB 왕복**(21번에서 이미 승격 권고, 이번에도 재확인):
   `holdOrderAssets` 26.67%, DB CPU 확정 병목. B-4식 왕복 병합이 후보.
3. ~~OutboxWriter 병렬 writer~~ — 이번 측정으로 **기각 확정**(위 판정 참고).
4. 도구 디스패치 지연 원인 조사(다음 벤치마크 세션 착수 전 우선 확인 권고).

## 범위 밖

- 후속 항목 1·2의 구현(식별·기록까지).
- WS 동시 시나리오(19·20번에서 규명 완료), 다중 심볼 스파이크.

## 원본 출력

### 측정 1 (A, batch=64)

```
checks_total.......: 933417  1451.47227/s (100.00% succeeded)
http_req_duration..: avg=1.33s min=4.01ms   med=1.39s max=3.53s p(90)=1.77s p(95)=1.96s
http_req_failed....: 0.00%  0 out of 939417
iterations.........: 933417 1451.47227/s
vus_max............: 3000
```

### 측정 2 (B, batch=512)

```
checks_total.......: 910105  1414.316014/s (100.00% succeeded)
http_req_duration..: avg=1.38s min=3.81ms   med=1.34s max=4.37s p(90)=1.99s p(95)=2.13s
http_req_failed....: 0.00%  0 out of 916105
iterations.........: 910105 1414.316014/s
vus_max............: 3000
```

### 측정 3 (조건부, batch=1024)

```
checks_total.......: 919110  1428.470384/s (100.00% succeeded)
http_req_duration..: avg=1.36s min=3.9ms    med=1.06s max=4.69s p(90)=2.2s  p(95)=2.31s
http_req_failed....: 0.00%  0 out of 925110
iterations.........: 919110 1428.470384/s
vus_max............: 3000
```

### 측정 4 (스파이크, batch=512)

```
checks_total.......: 100731  181.129376/s (100.00% succeeded)
checks_failed......: 0.00%   0 out of 100731

CUSTOM
custom_cancel_already_filled...: 8744   15.723017/s
custom_cancel_fail.............: 7959   14.31147/s
custom_cancel_success..........: 1404   2.524602/s
custom_market_success..........: 20191  36.306432/s
custom_order_success...........: 100731 181.129376/s

HTTP
http_req_duration..............: avg=5.14s min=1.45ms   med=1s    max=17.64s p(90)=14.32s p(95)=14.83s
http_req_failed.................: 6.37%  7959 out of 124838
http_reqs.......................: 124838 224.477361/s

iterations.........: 100731 181.129376/s
vus_max............: 3000
```

### pprof 상위 (측정 2, 30.16s 수집, 총 샘플 82.01s=272%)

```
      flat  flat%    cum   cum%
   15.47s 18.86%  15.47s 18.86%  internal/runtime/syscall.Syscall6
   ...
                  54.93s 66.98%  gorm.io/gorm.(*DB).Transaction
                  41.19s 50.23%  internal/handler.(*OrderHandler).CreateOrder
                  39.85s 48.59%  internal/service.(*OrderService).CreateOrder
                  21.87s 26.67%  internal/service.holdOrderAssets
                  16.26s 19.83%  internal/service.(*SettlementService).SettleTradeBatch
```
