# 21번째 테스트 (2026-07-17): B-3 심볼 샤딩 재측정 — 판정과 20번 오진의 정정

## 커밋 해시

- **A (before)**: `632455a` — 파이프라인 전환 직전(단일 엔진, 20번 측정 3과 동작 동일)
- **B (after)**: `16da453` — 현 main. B-3(ShardedEngine + 파이프라인 전환) 포함
  - 측정 2: `GOEXCHANGE_ENGINE_SHARDS=8` (기동 로그 `matching engine sharded: shards=8` 확인)
  - 측정 3(조건부): `GOEXCHANGE_ENGINE_SHARDS=1` (기동 로그 `shards=1` 확인)

## 왜 이 테스트를 했는지

[20번 벤치마크](20-2026-07-16-multi-symbol-remeasurement.md)는 "단일 엔진 goroutine의
직렬화가 처리량 캡(1,479 iter/s)"이라 판정했고, 그 근거로 B-3(심볼 샤딩)를
구현했다. 이번 측정은 그 판정의 검증이다 — 판정 신호 3개:
① 샤드별 order 채널이 1024 고정에서 풀리는가 ② 1,479 캡을 돌파하는가
③ 다음 병목은 무엇인가.

## 왜 이 방식을 선택했는지 (방법론 — 17~20번 계승)

- **같은 VM·같은 세션 연속 A/B**: 서버·DB VM은 세션 내내 stop/start 없음.
  A(측정 1) → B 샤드8(측정 2) → B 샤드1(측정 3), 매 측정 전
  TRUNCATE + backend 재시작 + `bootstrap loaded=0` 확인. 20번 수치와 직접 비교하지
  않는다(다른 세션) — 이번 세션의 측정 1이 기준선이다.
- **측정 3은 계획의 조건부 항목**: 측정 2가 A 대비 ±5% 이내(실측 −2.1%)라
  발동 — 라우터/팬인 오버헤드를 샤딩 병렬화 효과와 분리한다.
- **배포 함정 1건**: `docker-compose.stress.yml`의 environment 화이트리스트에
  `GOEXCHANGE_ENGINE_SHARDS` 전달 라인이 누락돼 있었다(B-3 구현 시 빠뜨림).
  첫 B 기동이 `shards=4`(기본 NumCPU)로 떠서 기동 로그 확인 규칙이 이를 잡았고,
  compose 파일에 `GOEXCHANGE_ENGINE_SHARDS: ${GOEXCHANGE_ENGINE_SHARDS:-}` 를
  추가해 해결했다(이 커밋에 포함 — 리포 갭 수정).
- 수집 5종(계획): 샤드별 채널 게이지 30초 샘플링(측정 2, 26회) / pprof CPU 30초
  (hold 중반) / DB VM CPU / settlement·outbox 지표 / load-gen 자원.

## 실행한 정확한 커맨드

19·20번과 동일한 리셋(TRUNCATE 9테이블)·배포(`docker compose -f
docker-compose.stress.yml up -d --build backend`)·부하(`k6 run -e
BASE_URL=http://10.10.0.3:8080 -e DEV_TOOLS_TOKEN=... ~/order-submission-multisymbol.js`,
8심볼 hold: 1.5분 워밍업 → VU 3000 8분 유지).

환경: 서버 `e2-highcpu-4`, DB `e2-highcpu-4`, load-gen `e2-standard-8`, k6 v2.0.0, 서울 리전.

## 핵심 결과

### REST 처리량

| # | 빌드 | 샤드 | iterations | 처리량(iter/s) | p95 | max | http_req_failed |
|---|---|---|---|---|---|---|---|
| 1 | A | (단일 엔진) | 966,971 | **1,498.47** | 1.81s | 4.60s | 0.00% |
| 2 | B | 8 | 942,890 | **1,467.15** (−2.1%) | **2.85s** (+57%) | 8.48s | 0.00% |
| 3 | B | 1 | 942,346 | **1,469.19** (−2.0%) | 1.80s | 4.87s | 0.00% |

### 판정 신호에 대한 답

**① 샤드 채널이 1024 고정에서 풀렸는가 — 아니오.** 측정 2에서 30초 간격 26회
샘플링 결과, **8개 샤드 전부가 hold 구간 내내 1024에 고정**(워밍업 초기만 0).
합산 execution 채널도 hold 내내 **8192(=8샤드×1024 전부 만석)**:

```
shard 0~7 order channel: avg 763~768, max 1024 (26샘플 — 워밍업 0 포함 평균)
merged execution(합산):  0,0,0,0,8192,8192,...(18회 연속),...,0,0
```

**② 1,479(이번 세션 기준 1,498) 캡을 돌파했는가 — 아니오.** 오히려 −2.1%.

**③ 다음 병목 — outbox writer→DB 구간으로 특정.** 측정 2 hold 중반 실측:
- DB VM: postgres **258%/400% CPU**, 호스트 idle 13% — DB가 포화 근접.
- 서버: backend 303%/400% CPU. pprof(30초, 총 269%): 매칭 `Match`는 **0.6%**(cum)로
  여전히 미미. 지배 경로는 `CreateOrder` 47.0%(그중 `holdOrderAssets` 23.5%),
  `SettleTradeBatch` 21.3%, 최상위 flat은 `syscall.Syscall6` 18.6%(DB 네트워크 왕복).
- **outbox flush: 8,655회에 471,143 이벤트 = 평균 54.4건/flush** — 배치 상한(64)에
  근접 포화 상태로 초당 15.2회(≈66ms/flush) 돌고 있었다. write-ahead 관문인
  OutboxWriter의 단일 배치 INSERT 루프가 이벤트 처리율을 캡하고 있다는 정량 증거.
- 정산 워커 큐 10개는 hold 중반에도 거의 빈 상태(2개만 24·256, 나머지 0) —
  조임목은 워커가 아니라 그 **앞단**(outbox 커밋)이다. 20번에서 관찰한 워커
  불균형은 병목이 아님도 함께 확인(백로그 승격 안 함).
- load-gen: 호스트 idle 84%, k6 RSS 11.6GB/31GB — 부하생성기는 병목 아님.

### settlement 지표 (측정 2)

배치 24,412개 / 471,143 체결 정산(평균 19.3건/배치), `fallbacks_total` 0.

## 해석

1. **B-3 판정: 성능 효과 없음 — 그리고 20번의 "엔진 직렬화 캡" 판정은 오진이었다.**
   샤드를 8배로 늘려도 8개 채널이 전부 다시 만석이 됐다는 것은, 적체의 원인이
   "엔진이 느려서"가 아니라 **"엔진이 방출한 이벤트를 다운스트림(outbox writer→DB)이
   못 받아서"**라는 뜻이다. 엔진의 `emitTrade`는 ExecutionCh에 블로킹 송신(A-3
   백프레셔 설계)하므로, outbox 커밋이 밀리면 → ExecutionCh 만석 → 엔진 블록 →
   order 채널 만석으로 **백프레셔가 역전파**된다. 20번 데이터에도 이 서명이 있었다
   (execution 채널 988~1024) — order 채널 적체를 엔진 직렬화로 해석하면서 그 함의를
   놓쳤다. **채널 게이지는 "어디서 막혔는지"가 아니라 "막혔다"만 보여준다 — 병목
   지목은 체인의 가장 아래(DB·flush 지표)부터 올라와야 한다**는 것이 이번 방법론적
   교훈이다.
2. **측정 3(샤드1 ≈ 샤드8, 0.1% 차)**: 남은 −2%는 샤딩 수와 무관한 상수 비용
   (팬인 홉 + 인터페이스 간접, 또는 run-to-run 노이즈 경계)이다. 라우터 자체는
   사실상 무해하다.
3. **단, 샤드8은 p95를 57% 악화시켰다(1.81s → 2.85s, max 8.48s).** 병목이
   다운스트림일 때 샤드 N개 = 주문 버퍼 N×1024로 깊어져, 같은 처리율에서 큐잉
   지연만 늘어난다(Little의 법칙). **병목이 엔진이 아닌 현재 상태에서 샤드>1은
   이득 없이 꼬리 지연만 키운다** — 기본 샤드 수(현재 `runtime.NumCPU()`)를 1로
   내리는 후속 조치를 권고한다(아래 후속 항목).
4. **B-3 코드 처분**: 오버헤드가 사실상 0(해석 2)이고 테스트로 검증된 구조이므로
   되돌리지 않는다. 매칭이 실제 CPU 병목이 되는 날(예: outbox 처리율 해소 후
   재측정)을 위한 기반으로 유지하되, README에는 "구현 완료·현 병목에선 효과
   없음(21번)"으로 정직하게 기록한다.
5. **에러율은 3개 측정 전부 0%, 정산 폴백 0.**

## 후속 항목 (로드맵 승격 후보 — 근거 포함)

1. **OutboxWriter 처리율** (승격 권고, 최우선): flush가 상한 64에 포화(54.4/64)
   상태로 66ms/flush — 배치 상한 상향 및/또는 심볼 파티션별 병렬 writer(순서 보장은
   심볼 내만 필요하므로 안전)로 write-ahead 관문을 넓힌다. 이번에 실측된 병목.
2. **주문 생성 경로 DB 왕복** (승격 권고): pprof의 최대 지분(47%, 지갑 홀드 23.5%).
   DB CPU 258%의 주요 소비자. B-4(정산 그룹커밋)와 같은 접근(왕복 병합)이 후보.
3. **기본 엔진 샤드 수 1로 하향**: 해석 3의 p95 악화 방지. env로 필요 시 확대.
4. ~~정산 워커 불균형~~ — 병목 아님 확인(워커 큐 상시 근공), 승격하지 않는다.

## 원본 출력

### 측정 1 (A, 단일 엔진)

```
checks_total.......: 966971  1498.467603/s (100.00% succeeded)
http_req_duration..: avg=1.28s min=3.48ms   med=1.33s max=4.6s  p(90)=1.7s  p(95)=1.81s
http_req_failed....: 0.00%  0 out of 972971
iterations.........: 966971 1498.467603/s
vus_max............: 3000
```

### 측정 2 (B, 샤드 8)

```
checks_total.......: 942890  1467.153993/s (100.00% succeeded)
http_req_duration..: avg=1.32s min=3.44ms  med=1.22s max=8.48s p(90)=2.44s p(95)=2.85s
http_req_failed....: 0.00%  0 out of 948890
iterations.........: 942890 1467.153993/s
vus_max............: 3000
```

hold 중반 자원(측정 2): 서버 backend 302.77% CPU(4 vCPU), DB postgres 257.95% CPU,
DB 호스트 `%Cpu: 51.1 us, 21.3 sy, 12.8 id`, load-gen 호스트 idle 84.4%.

### 측정 3 (B, 샤드 1)

```
checks_total.......: 942346  1469.185918/s (100.00% succeeded)
http_req_duration..: avg=1.32s min=3.55ms   med=1.36s max=4.87s p(90)=1.68s p(95)=1.8s
http_req_failed....: 0.00%  0 out of 948346
iterations.........: 942346 1469.185918/s
vus_max............: 3000
```

### pprof 상위 (측정 2, 30.17s 수집, 총 샘플 81.22s=269%)

```
      flat  flat%        cum   cum%
    15.09s 18.58%     15.09s 18.58%  internal/runtime/syscall.Syscall6
     0.94s  1.16%     15.05s 18.53%  runtime.mallocgc
     0.03s 0.037%     38.19s 47.02%  internal/handler.(*OrderHandler).CreateOrder
         0     0%     19.10s 23.52%  internal/service.holdOrderAssets
         0     0%     17.27s 21.26%  internal/service.(*SettlementService).SettleTradeBatch
     0.03s 0.037%      0.49s  0.60%  internal/matching.(*MatchingEngine).Match
```

## 범위 밖 (Out of Scope)

- 후속 항목 1~3의 구현(식별·기록까지가 이번 계획의 범위).
- WS 부하 시나리오(19·20번에서 규명 완료), 시장가 경로.
