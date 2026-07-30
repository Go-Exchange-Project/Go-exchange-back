# 정산 동시성 스윕 N=4 → 8 GCP 측정 runbook (30번)

> **For agentic workers:** 측정 runbook이다. Phase 0(preflight) → Phase 1(N=4 기준선 재현) →
> Phase 2(N=8) → Phase 3(무결성) → Phase 4(판정) → Phase 5(문서) 순서로 체크박스로 진행한다.
> **이 문서는 계획만 담는다 — GCP 실행은 별도 측정 세션에서 수행한다.**

**Goal:** 29번이 남긴 **"정상 구간의 커넥션 풀에 여유가 있으므로 N=4보다 높은 정산 동시성을
시험할 여지가 있다"** 는 가설을, **환경 차이가 `GOEXCHANGE_SETTLEMENT_CONCURRENCY` 하나뿐인
통제 비교**로 판정한다.

**완료 조건은 "N=8 실행 완료"가 아니다:**
> **N=4가 29번을 허용 오차 안에서 재현했는지, N=8이 안전 게이트를 통과했는지, 그리고 통과한
> 경우에만 처리율·지연·풀 고갈이 어떻게 변했는지를 구간별 수치로 판정한다.**

**선행**: [29번 결과](../../benchmarks/29-2026-07-30-per-order-fence-gcp-remeasurement.md)
("사후 분석 — DB 커넥션 풀 대기의 시간적 집중" 절) · [29번 runbook](2026-07-30-per-order-fence-gcp-remeasurement.md)(하니스 원본)

**배치 파편화 계측은 이 스윕 이후로 미룬다.** N=8만으로 충분히 개선되면 코드 변경을 더 늦출 수 있다.

---

## 측정 기준 SHA (고정)

```
82b4d7f6383676b11951fb81e8bea5e24eb73f59
```

- 29번과 **동일한 바이너리**다. 이후 커밋(`c05840b`, `1a5a63b`, `37c2940`)은 전부 문서 전용이며
  `git diff 82b4d7f..HEAD -- '*.go'`가 비어 있음을 실행 전 재확인한다.
- **이번 사이클에서 코드는 변경하지 않는다.** 계측 코드 추가가 필요하다고 판단되면
  **측정 세션에서 고치지 말고 중단·보고**한다.

## 비교 조건

**동일 고정** (29번과 같음): 서버·DB `e2-highcpu-4`(서울) · load-gen 2대 `e2-standard-8`
(A `OFFSET=0` / B `OFFSET=5000`, 각 `TOTAL_USERS=5000`) · 같은 `order-spike-availability.js` ·
같은 stage·VU·주문 비율 · `LOAD_START_AT_MS` 배리어 · `--summary-export` ·
`GOEXCHANGE_DB_MAX_OPEN_CONNS` 기본값 25 · 워터마크 0.75 · 파티션 10

**유일한 차이**

```
GOEXCHANGE_SETTLEMENT_CONCURRENCY = 4  (Phase 1)  →  8  (Phase 2)
```

**실행마다 DB를 초기화하고 서버를 재기동한다.** 두 실행이 상태를 공유하면 비교가 무의미해진다.

> **풀 크기(25)는 이번에 건드리지 않는다.** 변수를 둘로 늘리면 원인을 분리할 수 없다.
> N=8에서 풀이 제약으로 드러나면 **그 사실 자체가 결과**이고, 풀 상향은 다음 사이클이다.

---

## 이번에 새로 수집하는 것

29번에 없었던 두 가지다.

### 1. CPU 시계열 (서버·DB **분리**, 5초 간격)

**순간 `top` 한두 번으로 갈음하지 않는다.** 각 VM에서 실행 전체를 덮는 시계열을 남긴다:

```bash
loginctl enable-linger            # SSH 종료 시 systemd가 죽이는 문제(29번에서 겪음) 선행 조치
FILE=~/cpu-<role>-<phase>.txt     # role=server|db, phase=n4|n8 — 단계별 고유 파일
stdbuf -oL vmstat -t 5 > "$FILE" & echo $! > "$FILE.pid"
```

- **`stdbuf -oL`로 line buffering**을 건다. 리디렉션 시 블록 버퍼링이 걸리면 중단·크래시에서
  마지막 구간이 통째로 유실된다.
- 부하 시작 **전에 켜고** 드레인 **후에 끈다**. **램프 구간이 반드시 포함**돼야 한다.

**집계 규칙 — `us+sy`와 `100-id`는 동등하지 않다:**

| 판단 대상 | 컬럼 | 이유 |
|---|---|---|
| **CPU 병목** | **`us + sy`** | 실제로 CPU를 태운 시간 |
| **스토리지 대기** | **`wa`** | I/O wait는 CPU 포화가 아니다 |
| **호스트 경합(VM steal)** | **`st`** | 우리 워크로드가 아니라 GCP 호스트 쪽 문제 |

**`100-id`를 CPU 사용률로 쓰지 않는다** — `wa`와 `st`가 섞여 들어가 CPU 병목을 과대평가한다.
**세 값을 각각 max / p95 / median으로 보고**한다.

**`vmstat`의 첫 표본은 부팅 이후 누적 평균이므로 분포 계산에서 제외한다**(포함하면 max·p95·median이
왜곡된다). 파싱 시 헤더 2줄과 **첫 데이터 행 1줄**을 버린다.

### 1-a. 수집기 수명 관리 (N=4 수집기가 N=8까지 살아남는 것을 막는다)

```
시작 전:  기존 수집기 0개 확인   pgrep -af '[v]mstat -t 5'   → 결과 없음
시작:     단계별 고유 파일 + PID 파일 기록
종료:     kill "$(cat "$FILE.pid")"
종료 확인: kill -0 "$(cat "$FILE.pid")" 가 실패(=프로세스 없음),
          그리고 pgrep -af '[v]mstat -t 5' → 결과 없음,
          파일 크기·마지막 타임스탬프 확인
```

**패턴을 `'[v]mstat -t 5'`로 쓰는 이유**: `pgrep -f "vmstat -t 5"`는 **원격 셸이 실행 중인
자기 명령 문자열까지 매칭**해(특히 `ssh ... 'pgrep -f "vmstat -t 5"'` 형태) 수집기가 없는데도
결과가 나온다. 대괄호 트릭이 자기 매칭을 피한다. **더 확실한 검증은 PID 파일 기반
`kill -0`** 이므로 둘을 함께 쓴다.

**단계마다 파일명이 달라야 하고**(`n4`/`n8`), **종료 확인까지 해야** 두 실행의 시계열이 섞이지 않는다.

### 2. 풀 지표의 **구간별 델타**

`go_sql_in_use_connections` / `go_sql_wait_count_total` / `go_sql_wait_duration_seconds_total`을
**램프 · 평시 · 버스트 · 회복** 네 구간으로 나눠 계산한다:

- `wait_count` · `wait_duration`은 **누적 카운터** → 구간 시작·종료 스냅샷의 **델타**
- `in_use`는 **게이지** → 구간별 **max / p95 / median**(단일 시점으로 대표하지 않는다)

29번 기준선(같은 방식으로 계산한 값):

| 구간 | `in_use` max / p95 / median | 비고 |
|---|---|---|
| 램프 | **25 / 25 / 25** | 상한 25 포화 |
| 이후 | **8 / 6 / 0** | 여유 |

누적 대기의 **99.6%가 초기 약 7.5분에 집중**됐고 이후 15분간 `wait_count`는 +114건(+0.4%)뿐이었다.

---

### Phase 0: Preflight (저부하 1~2분 — 실패 시 전체 실행 금지)

- [ ] **Step 1: 코드 동일성** — 배포 커밋이 측정 기준 SHA `82b4d7f`와 일치하고
  `git diff 82b4d7f..HEAD -- '*.go'`가 비어 있음을 확인.
- [ ] **Step 1-a: DB 접속 유효성** — 현재 `go-exchange-back` 환경을 canonical source로 사용.
  **과거 `bench-*` 값을 복사하지 않는다.** 값 비교가 아니라 **실제 연결 성공**으로 검증하고,
  **비밀번호 값·hash·fingerprint를 로그·문서·아티팩트에 출력하지 않는다.**
- [ ] **Step 1-b: 스키마 확인 (TRUNCATE보다 먼저)** — `failed_order_cancellations`(존재 +
  `order_id` uniqueIndex + `retry_count` default 0 + CHECK `>= 0`), `failed_market_completions`
  (default 0, 신 제약 존재 + 구 제약 `_retry_count_positive` 부재). **CHECK/default까지** 확인한
  뒤에야 10개 테이블 TRUNCATE.
- [ ] **Step 1-c: Secret preflight** — [29번 runbook](2026-07-30-per-order-fence-gcp-remeasurement.md)의
  절차를 그대로 따른다:
  - **기존(폐기된) 토큰이 거부**되는지 확인
  - **현재 secret이 인증 단계를 통과**하는지 확인
  - **토큰 값·hash·fingerprint를 출력하지 않는다**(문서·아티팩트·로그 전부)
  - **실패 시 부하 실행 금지**
- [ ] **Step 1-d: CPU 수집기 동작 확인** — 서버·DB 양쪽에서:
  - 시작 전 `pgrep -af '[v]mstat -t 5'` **결과 없음**(이전 단계 수집기 잔존 없음 —
    대괄호 트릭으로 자기 명령 매칭을 피한다)
  - `stdbuf -oL`로 띄운 뒤 출력이 **실시간으로 쌓이는지**(파일 크기 증가 확인)
  - **SSH를 끊어도 계속 도는지**(`enable-linger`)
  - `kill "$(cat "$FILE.pid")"` 후 **`kill -0`이 실패**하고 `pgrep -af '[v]mstat -t 5'`도
    결과 없음까지 확인
- [ ] **Step 1-e: 시간 정렬** — CPU 시계열과 Prometheus 구간을 맞추려면 시계가 맞아야 한다:
  - **서버·DB·load-gen 2대의 UTC 시계 차이 ≤ 1초**(`date -u`). 초과하면 원인 해결 후 재개.
    근거: CPU 표본 간격이 5초이고 load-gen 시작 skew 계약도 ≤1초이므로, 1초를 넘는 드리프트는
    구간 귀속을 한 표본 이상 어긋나게 만든다
  - **각 단계(램프·평시·버스트·회복)의 UTC 시작·종료 시각을 기록**해 둔다 —
    이 기록 없이는 CPU max/p95를 구간별로 귀속시킬 수 없다
  - load-gen 두 대의 시작 skew ≤ 1초(29번과 동일)
- [ ] **Step 2: 저부하 실행** 후 전부 확인:
  - 기동 로그가 **의도한 `concurrency=N`** 을 출력(Phase 1은 4, Phase 2는 8)
  - `/metrics`에 `go_sql_*` 3종이 노출
  - `settlement_terminal_wait_seconds{kind}` · `settlement_outstanding_jobs{partition}` ·
    `settlement_quarantined_orders{partition}` · `settlement_dependency_record_failed_total` ·
    `settlement_duplicate_terminal_total` 노출
  - 두 load-gen 시작 skew ≤ 1초
  - **dispatch wait count = execution count** — Phase 3과 **동일한 제한**: 저부하 종료 후
    드레인이 끝나고 `settlement_outstanding_jobs`가 **모든 파티션에서 0**인 최종 스냅샷에서만
    검사한다(도중에는 in-flight 때문에 어긋나는 것이 정상이다)
  - 기존 SLI·정합성 검사 정상
- [ ] **Step 3: 게이트** — 하나라도 어긋나면 **전체 실행 금지**. 원인 해결 후 Phase 0 재수행.

---

### Phase 1: N=4 기준선 재현 (**게이트**)

`GOEXCHANGE_SETTLEMENT_CONCURRENCY=4`. DB 초기화 + 서버 재기동 후 29번과 동일 프로파일로 실행.

- [ ] **Step 1: 실행** — CPU 수집기 기동 → 부하 → 드레인 → 수집기 정지
- [ ] **Step 2: 29번 대비 재현 확인**

| 지표 | 29번 (hold) | 허용 오차 |
|---|---|---|
| settled trades (/s) | 371.7 | ±10% |
| DB attempt 빈도 (/s) | 133.64 | ±10% |
| worker busy ratio | 99.95% | ±2%p |
| 평균 trade batch | 2.781 | ±10% |
| `sli_order_business_success` | 22.70% | ±2%p |
| `outstanding_jobs`가 `2N`인 샘플 비율 | 100.0% | ±5%p |

> **이 허용 오차는 측정된 분산에서 유도한 값이 아니다.** 동일 구성을 두 번 돌린 적이 없어
> run-to-run 분산을 모르기 때문이다. **이번 N=4 재현이 곧 그 분산의 첫 측정**이다.
> 재현이 오차를 벗어나면 그 자체가 결과다 — **"단일 실행 A/B 비교가 성립하지 않는다"**
> 는 뜻이므로 **N=8 비교를 중단하고 분산 문제부터 보고**한다.

- [ ] **Step 3: 게이트** — 재현 실패 시 **Phase 2 진행 금지**.

---

### Phase 2: N=8

`GOEXCHANGE_SETTLEMENT_CONCURRENCY=8`. **DB 초기화 + 서버 재기동**. 그 외 전부 동일.

- [ ] **Step 1: 기동 로그에서 `concurrency=8` 확인**(env가 실제로 먹었는지)
- [ ] **Step 2: 실행** — Phase 1과 같은 절차·같은 수집
- [ ] **Step 3: 원본 보존** — 스냅샷·k6 stdout·`summary-*.json`·`vmstat` 파일·시각 기록을
  `_workspace/concurrency-sweep-30/`에. **토큰·외부 IP 제거.**

---

### Phase 3: 무결성 (결과 해석 **전에** 검증)

29번 Phase 3의 9항목을 **양쪽 실행 모두**에 적용한다:

- [ ] `settlement_terminal_wait_seconds{kind}` count = 정상 처리된 terminal 수
- [ ] **dispatch wait count = job execution count** — **부하 종료 후 드레인이 끝난 최종 스냅샷에서만
  검사한다.** 부하 도중에는 정상적인 in-flight job 때문에 두 값이 어긋나는 것이 당연하다.
  **검사 전제**: `settlement_outstanding_jobs{partition}`가 **모든 파티션에서 0**이고 드레인이
  완료됐음을 먼저 확인한다. 이 전제 없이 중간 스냅샷으로 비교하면 정상 상태를 계측 결함으로 오판한다
- [ ] batch attempt count ≥ logical batch job count, 차이가 실제 retry 로그와 일치
- [ ] settlement batch의 trade 합계 = settled trade 증가량
- [ ] k6 주문 합계 = 서버·DB 주문 합계
- [ ] `settlement_quarantined_orders` 종료 시 0
- [ ] `settlement_duplicate_terminal_total` = 0
- [ ] CPU 시계열이 **부하 구간 전체를 덮는지**(램프 포함, 끊긴 구간 없음)
- [ ] CPU 파싱에서 **첫 데이터 행(부팅 이후 누적 평균)이 제외**됐는지
- [ ] 각 단계의 **UTC 시작·종료 시각 기록**이 있고, CPU 시계열·Prometheus 스냅샷 시각과 정렬되는지
- [ ] N=4·N=8 수집기 파일이 **단계별로 분리**돼 있고 프로세스가 겹치지 않았는지
- [ ] 하나라도 불일치면 **계측 문제로 보고 판정 보류**

---

### Phase 4: 판정 — **안전이 성능보다 먼저**

- [ ] **Step 1: 안전 게이트 (하나라도 악화되면 성능 수치를 읽기 전에 기각)**

| 게이트 | 기준 |
|---|---|
| 정합성 위반 | **0** |
| `settlement_batch_fallbacks_total` | **0** |
| `sli_cancel_success` | **100%** 유지 |
| `sli_order_response_availability` | **100%** 유지 |
| quarantine 잔존 · duplicate terminal | **0** |

> **N=8은 락 경합·데드락 표면을 넓힌다.** 위 중 하나라도 나빠지면 **처리율이 올랐더라도 기각**한다.
> 정합성은 비협상이다.

- [ ] **Step 2: 핵심 판정 — 초기 풀 고갈이 더 심해졌는가**

29번에서 **램프 구간의 풀은 이미 25/25 포화**였다. N=8은 정산 워커를 4개 더 얹으므로
**같은 25개 커넥션을 두고 경쟁이 심해질 수 있다.** 전체 처리율만 보고 판정하면 이걸 놓친다.

| 비교 | N=4 | N=8 | 판정 |
|---|---|---|---|
| **램프 구간** `wait_count` 델타 | | | 증가 폭 |
| **램프 구간** `wait_duration` 델타 | | | 증가 폭 |
| 램프 `in_use` max/p95/median | | | 포화 지속 시간 |
| 평시 `in_use` max/p95/median | | | 여유 소진 여부 |
| HTTP `max` 지연 | | | 램프 고갈의 대리 지표 |

- [ ] **Step 3: 성능 판정** (안전 게이트 통과 후에만)

| 항목 | 기준 |
|---|---|
| 처리율 | settled trades/s · DB attempt/s 증가 여부 |
| 업무 성공률 | `sli_order_business_success` 22.70% 대비 |
| 응답 지연 | p90 · p95 · max (29번: 62.2 / 120.5 / 4,555~4,800ms) |
| CPU | 서버·DB 각각 **`us+sy` / `wa` / `st`를 따로** max·p95·median — **어느 쪽이 먼저 벽에 닿는지**. `us+sy`가 낮은데 `wa`가 높으면 CPU가 아니라 스토리지, `st`가 높으면 우리 워크로드가 아니라 호스트 경합이다 |
| worker busy | 99.95%가 유지되는지(N=8에서도 포화면 여전히 워커가 상한) |
| `outstanding_jobs` | `2N`(=16) 점유율 |

**해석 주의**: 업무 성공률이 오르면 셰딩이 줄어 **느린 요청의 비중이 커지므로 p90·p95는
구성 효과만으로도 올라간다.** 지연 악화를 처리율 회귀로 오독하지 않는다.

---

### Phase 5: 문서 + 안전 종료

- [ ] **문서** — `docs/benchmarks/30-<날짜>-settlement-concurrency-sweep.md`:
  비교 조건 동일성 / N=4 재현 검증 / 구간별 계산표 / **안전 게이트 결과** / 램프 풀 고갈 비교 /
  CPU max·p95·median / 성능 판정 / 한계.
- [ ] **README**·완료 문서 갱신.
- [ ] **Commit + 푸시 + CI green**.
- [ ] **VM 4대 정지 후 `gcloud compute instances list`로 `TERMINATED` 확인** —
  "정지 명령을 실행했다"가 아니라 **조회 결과**가 완료 조건이다.

```bash
gcloud compute instances stop goexchange-stress-server goexchange-stress-db goexchange-stress-load-gen goexchange-stress-load-gen-b --zone asia-northeast3-a
```

---

## 판정이 나온 뒤

| 결과 | 다음 |
|---|---|
| **N=8이 안전 통과 + 처리율 개선** | 코드 변경 없이 이득 — **배치 파편화 수정을 더 미룰 수 있다.** N=16 시험 여부는 CPU 여유로 판단 |
| **N=8이 안전 통과 + 개선 없음** | 워커 수가 제약이 아니다 → **배치 파편화 진단으로 이동**(run-length 분포 + job 종류별 실행 시간) |
| **N=8이 안전 게이트 실패** | 즉시 N=4로 되돌리고 원인 규명. 락 경합이면 그것이 다음 표적 |
| **램프 풀 고갈 악화가 지배적** | 풀 크기(25)가 다음 변수 — 단 **한 번에 하나씩** |

## 범위 밖

- **배치 파편화 수정**(`collectTradeBatch`가 terminal을 건너뛰기 / terminal batching) — 이 스윕 결과 이후
- `settlement_job_execution_seconds`의 job 종류별 라벨 추가 — **코드 변경이므로 이번 사이클 아님**
- `GOEXCHANGE_DB_MAX_OPEN_CONNS` 조정 — 변수를 둘로 늘리지 않는다
- 축 2(매칭 quantum) — 독립 판정
- `settlement_terminal_wait_seconds{cancel}` 버킷 하한 축소
