# 34 (2026-08-18): `trades.buy_order_id` 인덱스 단독 수정·재측정

> **설계**: [2026-08-11-buy-order-index-remeasurement-design.md](../superpowers/specs/2026-08-11-buy-order-index-remeasurement-design.md) ·
> **계획**: [2026-08-11-buy-order-index-remeasurement.md](../superpowers/plans/2026-08-11-buy-order-index-remeasurement.md)
> **선행**: [32-B 용량 경계](32-2026-08-01-capacity-boundary-session-b.md) · [33 정산 비용 성장 진단](33-2026-08-02-settlement-cost-growth-diagnostic.md)

## 1. 사전 등록된 판정과 최종 결론

임계값과 실패 분기는 **측정 전에** 설계 문서에 고정했고, 결과를 본 뒤 바꾸지 않았다.

| 게이트 | 사전 등록 기준 | 결과 |
|---|---|---|
| 로컬 6셀 × 2회 | `market_slope` N=1·N=8 모두 **≤ 1.40**, mid/full 인덱스 사용, 12셀 정합성 0 | **PASS** |
| GCP 500 (1회) | `job_growth ≤ 1.40` **및** 100%/0건 계약 7항목 | **PASS** |
| GCP 750 r1 | 500의 2~7번 조건(hard gate) | **PASS** |
| GCP 750 r2 (독립 확증) | 〃 | **FAIL** |

판정표에서 도달한 행은 **3행**이다.

| 로컬 | GCP 500 | GCP 750 | 고정 결론 |
|---|---|---|---|
| PASS | PASS | **한 실행 이상 FAIL** | **비용 개선은 유효하나 검증 경계는 500** |

> **최종 결론**
> **인덱스는 비용 성장의 지배적 항을 제거하고 750 VU 실패 규모를 약 18배 줄였지만,
> 독립 확증 실행이 실패했으므로 유한 실행으로 검증된 최고 용량은 500 VU다.**
>
> 750으로 경계가 이동했다고 쓰지 않는다. 두 실행을 평균하지도 않는다.

### 비교 조건

| 항목 | 값 |
|---|---|
| 후보 SHA | `fbd751710aec87c9331851f49c671bc566d867ba` |
| 기준 이미지 | `sha256:37a26b56ff38…ed56` (32-B가 쓴 그 이미지) |
| 실행 이미지 | `sha256:59c5e8491c8e…5856` (**기준 이미지 + migration 006 한 레이어**) |
| 레이어 | base 6 → derived 7 (**delta 1**) |
| `/app/go-exchange-back` SHA-256 | `d485bd1ccd6b…bfba` — **base = derived 완전 일치** |
| migrations 001~005 | 5개 해시 **전부 일치**, 006은 파생에만 |
| 구성 | Server `e2-highcpu-4` · DB `e2-highcpu-8` · concurrency **8** · 파티션 10 · pool 25 |
| k6 | **v2.1.0+dirty (snap rev 56)** — 32-B와 동일 |
| 워크로드 | 32-B와 동일, k6 스크립트 3개 sha256 일치 |

**전체 재빌드를 하지 않은 이유**: 바이너리를 바이트 단위로 보존해야 이번 측정의 유일한 런타임
변수가 인덱스 하나가 된다. Dockerfile이 `COPY migrations /app/migrations`로 migration을 이미지에
굽기 때문에 32-B 이미지를 그대로 쓰면 006이 적용되지 않는다. 그래서 **파생 이미지**를 만들었다.

---

## 2. 원본 결과

### 2.1 로컬 6셀 × 2회 (1차 게이트)

주 판정값은 **두 회차 p50의 중앙값 비**다. 33번에서 같은 셀의 두 회차가 원시 기준 최대 34%
차이 났기 때문에 회차 하나를 그대로 쓰지 않는다.

| 종류 | N=1 | N=8 | 33번(수정 전) N=1 / N=8 |
|---|---|---|---|
| **market_terminal** | **0.998** | **1.077** | **4.885 / 5.724** |
| trade_batch | 1.024 | 1.079 | 1.258 / 1.284 |
| cancel_terminal | 1.047 | 1.073 | 1.281 / 1.334 |

회차별 원시 비율도 전부 임계값 미만이라 회차 불일치 한계는 발생하지 않았다
(`raw_run_disagreement_limitation: false`).

| | run1 | run2 |
|---|---|---|
| market N=1 | 1.0001 | 0.9969 |
| market N=8 | 1.1215 | 1.0345 |

**실행계획 — 12/12 셀에서 Seq Scan이 사라지고 `idx_trades_buy_order_id`가 사용됐다.**
다만 스캔 유형은 균일하지 않다.

| 셀 | 스캔 유형 | Aggregate 비용 | shared hit |
|---|---|---|---|
| mid·full **8셀** | **Index Scan** | 8.45 | 5 |
| `initial/N1` **2셀** | **Index Scan** | 8.30 / 8.31 | 3 |
| `initial/N8` **2셀** | **Bitmap Index Scan** | 11.07 / 11.04 | 3 |

`full/N8`은 Index Scan 노드 총비용 **8.44**, Aggregate 총비용 **8.45**, `shared hit=5`다.
`initial`은 `trades`가 비어 있어 비용·버퍼가 다르다.

**카탈로그 증거** (`full/N8`, 33번 대비):

| 지표 | 33번 | 34번 |
|---|---|---|
| `trades` seq scan | 707회 | **0** |
| seq 튜플 읽기 | 85,049,132 | **0** |
| 대상 쿼리 평균 | 115.16ms | **0.040ms** |
| DB 실행시간 비중 | 86.1% | **0.9%** |

12셀 정합성 위반 **0**.

> **부수 관측 — 블록 접근이 87% 줄었다.** `full/N1`의 `blks_hit + blks_read`가
> 33번 1,841,278 → 34번 230,426(**−87.49%**)이다. 대조군인 `initial/N1`은 209,347 → 206,284로
> 사실상 같다(`trades`가 비어 인덱스가 무의미한 셀). 세션 간 환경 드리프트가 아니라
> 전수 스캔 제거의 효과로 읽힌다. `shared_buffers` 128MB에 `full`의 `trades`가 약 100MB이므로
> 매 스캔이 캐시를 밀어냈다는 설명과 정합적이지만, **통제 실험으로 분리하지는 않았다.**

### 2.2 GCP 500 VU 10분 (2차 게이트) — PASS

| 항목 | A | B |
|---|---|---|
| 응답 가용성 | **100%** (212,489 / 0) | **100%** (212,597 / 0) |
| 업무 성공률 | **100%** (212,489 / 0) | **100%** (212,597 / 0) |
| 취소 성공률 | **100%** (15,371 / 0) | **100%** (15,197 / 0) |
| 1초 초과 | **0건** | **0건** |

- 셰딩(`orders_admission_rejected_total`) **카운터 미노출 = 0건**
- fallback · completion blocked · dependency record failed · duplicate terminal **전부 0**
- job `failed`/`fallback` **0** (success 224,385), `outstanding`/`quarantined` 최댓값 **0**
- 무결성 SQL 4항목 **0**, outbox PENDING 잔여 **0**
- 재기동 리컨실리에이션 `ts=1786974026 ≥ restart_epoch=1786974018`, **4항목 0**
- outbox replay `replayed=0 deferred=0 corrupted=0`, stale market orders `0/0`

**`job_growth_500 = 1.078`** (임계 1.40) — first `6.794ms`(22,143 job) → last `7.327ms`(21,242 job).

처리량: 주문 425,086 · 체결 241,487 · ledger 1,464,354행.

### 2.3 GCP 750 VU r1 — hard gate PASS

| 항목 | A | B |
|---|---|---|
| 응답 가용성 | **100%** (317,777 / 0) | **100%** (317,227 / 0) |
| 업무 성공률 | **100%** (317,777 / 0) | **100%** (317,227 / 0) |
| 취소 성공률 | **100%** (22,616 / 0) | **100%** (22,657 / 0) |
| 1초 초과 | **0건** | **0건** |

셰딩 0, 정합성 4항목 0, 재기동 리컨실리에이션 `ts=1786976593 ≥ 1786976577` 4항목 0.
`job_growth_750 = 1.256` (설명 지표, PASS threshold 아님) — first `7.628ms` → last `9.581ms`.

처리량: 주문 635,004 · 체결 361,043 · ledger 2,188,704행.

### 2.4 GCP 750 VU r2 (독립 확증) — hard gate FAIL

| 항목 | A | B |
|---|---|---|
| 응답 가용성 | **100%** (314,801 / 0) | **100%** (314,565 / 0) |
| **업무 성공률** | **99.687%** (313,816 / **985**) ❌ | **99.670%** (313,526 / **1,039**) ❌ |
| 취소 성공률 | **100%** (22,472 / 0) | **100%** (22,820 / 0) |
| 1초 초과 | **0건** | **0건** |

셰딩 **2,024건** — `orders_admission_rejected_total{stage="engine_gate"} 2024`가
k6 A+B 실패 합(985 + 1,039)과 **정확히 일치**한다.

**정합성은 깨끗하다** — `failed_settlements` / `failed_order_cancellations` /
`failed_market_completions` / `reconciliation_violations` **전부 0**, job `failed`/`fallback` 0,
`outstanding`/`quarantined` 0, 재기동 리컨실리에이션 `ts=1787057576 ≥ 1787057564` 4항목 0.

즉 **가용성·정합성·취소가 아니라 업무 성공률 100% 기준 단독으로 탈락했다.**
`job_growth = 1.462` — first `7.525ms` → last `11.000ms`.

처리량: 주문 627,342 · 체결 356,130 · ledger 2,160,684행.

---

## 3. 32-B 대비 효과와 job mix

### 3.1 같은 750 VU에서의 거동

| 750 VU | 32-B (인덱스 없음) | 34 r1 | 34 r2 |
|---|---|---|---|
| 업무 성공률 | **93.484%** | 100% | 99.67% |
| 셰딩 | **36,989** | 0 | **2,024** |
| 셰딩 시작 | hold **4.2~4.4분** | 없음 | 말미 |
| `job_growth` | **2.58** | 1.256 | 1.462 |
| first → last 분 | 9.19 → 23.75ms | 7.63 → 9.58ms | 7.53 → 11.00ms |

**셰딩이 36,989 → 2,024로 약 18배 줄었다.** 500에서는 `job_growth 1.078`로 사실상 평평하다.

### 3.2 job mix — 32-B와 동일

| | 32-B 750 | 34 500 | 34 750 r1 | 34 750 r2 |
|---|---|---|---|---|
| trade | 114,873 | 103,071 | 137,505 | 132,157 |
| cancel | 37,069 | 29,116 | 43,097 | 43,140 |
| market_done | 102,674 | 80,719 | 120,590 | 119,208 |
| **market / 전체** | 40.3% | 37.9% | 40.0% | **40.5%** |
| **market / terminal** | **73.5%** | 73.5% | 73.7% | **73.4%** |

`terminal_boundary_difference`(dispatch 기준 − worker 완료 기준)는 각각 −4 / −1 / −1로
1분 경계 in-flight 차이 수준이다.

**job 구성이 32-B와 사실상 같으므로, 개선을 mix 이동으로 설명할 수 없다.**

---

## 4. 귀속 한계

**말할 수 있는 것**

- 로컬 6셀에서 `market_terminal`의 크기 기울기가 **4.885/5.724 → 0.998/1.077** 로 사라졌고,
  대상 쿼리의 seq scan이 12/12 셀에서 없어졌다
- GCP 500에서 100%/0건 계약을 전부 지키며 `job_growth`가 **1.078** 이다
- 같은 750 VU에서 셰딩이 **36,989 → 2,024**, `job_growth`가 **2.58 → 1.256/1.462** 다
- 런타임 변경은 **migration 006 한 레이어**뿐이다(바이너리·워크로드·k6·구성 동일)

**말할 수 없는 것**

- **검증된 최고 용량이 750으로 이동했다** — r2가 실패했다. 최고 검증 용량은 **500 VU**다
- **500 VU의 32-B 시절 `job_growth`** — 측정되지 않았다. 따라서 "인덱스가 2.58을 1.078로
  만들었다"고 쓸 수 없다. 비교 가능한 것은 **같은 750 VU에서의 배율 변화**까지다
- **500과 750 사이의 실제 경계** — 해상도 250 VU이고 그 사이는 측정하지 않았다
- **750이 왜 실행마다 갈리는지** — r1과 r2는 job mix가 같은데 `job_growth`가 1.256 vs 1.462였다.
  750이 이 구성에서 경계에 걸쳐 있다는 것까지가 관측이고, 원인은 규명하지 않았다
- **32-B의 남은 비용 성장 원인** — 33번이 배제하지 못한 WAL·스토리지·autovacuum·락 경합은
  이번에도 분리하지 않았다. 인덱스가 **지배적 항**을 제거했다는 것이지 유일한 항이었다는 뜻이 아니다

---

## 5. 폐기한 시도와 실행상 함정

### 5.1 폐기된 실행 — 결과에서 완전히 제외

**`index500r1` 1차 시도**는 k6가 **초기화 단계에서 즉시 실패**해 폐기했다.

```
could not initialize 'order-spike-availability.js':
The moduleSpecifier "./sli-classify.js" couldn't be found on local disk
```

- k6 종료 `13:07:10Z` / hold 시작 예정 `13:12:26Z` → **hold 진입 전**
- DB `orders`/`trades`/`wallets`/`users` **전부 0** (setup 도달 전, 부하 미발생)
- collector 산출물은 `preflight-index500r1-*`로 별도 보존, append 없이 새 파일로 재시작

**이 시도의 어떤 수치도 위 결과에 포함되지 않는다.**

원인은 **계획 Task 5 Step 1의 복사 목록 결함**이다. k6 스크립트의 로컬 import
`sli-classify.js` · `level-classify.js` 두 개가 목록에 없었다. 32-B 문서는 이 두 파일의
sha256을 기록해 뒀는데 계획서가 이를 옮기지 않았다.

### 5.2 부하 전에 잡은 함정 (전부 계획서에 없던 것)

| 함정 | 실제 상태 | 처리 |
|---|---|---|
| **k6 자동 갱신** | snap이 v2.1.0 → **v2.2.0**으로 올려놓음 | `snap revert`로 rev 56(v2.1.0) 복원 + `refresh --hold`. 측정 도구가 바뀌면 32-B 비교가 성립하지 않는다 |
| **`.env` concurrency** | 파일은 **4**, 32-B 실제 실행은 **8** | `.env`를 신뢰하지 않고 셸 환경으로 8 명시 + **3단계 검증**(compose config → 컨테이너 env → 기동 로그) |
| **load-gen 토큰** | 양쪽 모두 **낡은 값**(서버와 불일치) | 서버 `.env` 값으로 파일 릴레이 갱신. 값·해시 미출력, `cmp` 결과만 기록 |
| **`Linger=no`** | VM 재시작으로 초기화 | `enable-linger` 재설정. 없으면 SSH 종료 시 k6가 죽는다 |

**32-B의 concurrency=8은 `.env`가 아니라 기동 로그로 확인했다** —
`cap750r1-backend-err.log`의 `settlement partitions=10 concurrency=8`. 로컬 32-B 스냅샷의
`settlement_outstanding_jobs` 최댓값 **16 = 2N**으로 교차검증했다.

### 5.3 절차 변경

**DB를 `docker compose up`이 아니라 `docker start`로 올렸다.** DB compose 디렉터리
(`/home/goexchange/go-exchange-db`)에 `.env`가 없어 재생성 시 `POSTGRES_PASSWORD` 보간이
불가능했다. 32-B와 동일한 컨테이너 설정을 보존하기 위해 기존 컨테이너를 그대로 기동했고,
image ID · 환경 · mount · port 구성을 측정 전에 캡처했다.

```
image   postgres:18-alpine  sha256:9a8afca54e78…de15
env     POSTGRES_DB=goexchange, POSTGRES_USER=goexchange, PASSWORD=<redacted>, PG_VERSION=18.4
mount   volume goexchange-db_goexchange-db-postgres-data -> /var/lib/postgresql (rw)
port    5432/tcp -> 0.0.0.0:5432
cmd     postgres -c shared_preload_libraries=pg_stat_statements -c pg_stat_statements.track=all
```

### 5.4 도구 버그

**`docker exec -i`가 heredoc 스크립트의 나머지를 stdin으로 먹었다.** 원격 점검 스크립트에서
첫 psql 호출이 뒤 명령들을 삼켜 출력이 조용히 잘렸다. 조회에는 `-i`를 쓰지 않고
`</dev/null`을 붙여 고쳤다. **이 버그로 잘린 확인은 전부 다시 수행했다.**

또한 리컨실리에이션 대기 루프가 `1.786974026e+09`(과학표기)를 정수 비교하지 못해 계속 돌았다.
`awk '{printf "%d"}'`로 고쳤다. **판정기(`hard_gate.py`)는 `float()`로 파싱하므로 영향 없다.**

---

## 6. 산출물 · 증거

### 6.1 판정기 사전 검산

측정 전에 판정기를 고정하고 **기존 원본으로 검산**했다.

- `local_gate.py` × 33번 원본 → 여섯 기울기가 계획 기댓값과 소수 셋째 자리까지 일치
  (`1.258 / 1.284 / 1.281 / 1.334 / 4.885 / 5.724`), exit 1
- `gcp_gate.py` × 32-B `cap750r1` → `9.2199 → 23.7485ms`, growth `2.5758`,
  market 전체 `40.33%` · terminal `73.47%`, `terminal_boundary_difference = 2`
- `test_gates.py` 16개 단위 테스트 통과

### 6.2 원본 위치

```
_workspace/buy-order-index-remeasurement/
  local/run1, local/run2          로컬 12셀 JSON
  local-gate.json                 1차 게이트 판정
  scripts/                        하니스·판정기 + script-sha256.txt
  gcp/index500r1, index750r1, index750r2
      run.json, summary-a/b.json, metrics-final.txt,
      metrics-postrestart.txt, integrity.txt,
      hard-gate.json, gcp-gate.json|gcp-report.json,
      snapshots/ (176 / 98 / 75), cpu-*.txt (각 4)
  gcp/ARTIFACT-SHA256.txt         산출물·하니스·판정기 checksum
  redaction-manifest.json         setup_data 제거 이력(정리 전/후 checksum)
```

> **⚠ 산출물은 측정 후 재패키징됐다 — 지표는 불변이다.**
>
> k6의 `--summary-export`는 `setup_data`를 그대로 덤프하는데, 이 하니스의 `setup()`은
> 부하 사용자별 JWT를 그 안에 담는다. 그래서 평문 summary 12개와 load-gen tgz 6개에
> 사용자 토큰이 남아 있었다(총 6,000개, 중복 사본 포함).
>
> 2026-08-23에 **`setup_data`를 `{users_redacted, note}`로 치환**하고 load-gen tgz를
> 재패키징했다. **`metrics`를 포함한 지표 데이터는 건드리지 않았고**, 정리 전후 18개
> 파일의 `metrics`가 동일함을 파싱 비교로 확인했다. DB·server tgz 6개는 열지 않았다
> (스캔 결과 토큰 0건).
>
> 위 `ARTIFACT-SHA256.txt`의 `summary-a/b.json` 6줄은 **정리본 기준으로 갱신**됐고,
> 정리로 값이 바뀐 load-gen tgz 6개의 checksum을 새로 추가했다. 나머지 항목은 측정 당시
> 값 그대로다(전수 대조 41건 일치). 정리 전 checksum과 제거 필드는
> `redaction-manifest.json`에 남겼다. **JWT를 포함한 원본은 보존하지 않는다.**

### 6.3 GCP 종료

정지 명령 실행이 아니라 **조회 결과**가 완료 조건이다.

```
goexchange-stress-db          asia-northeast3-a  e2-highcpu-8   TERMINATED
goexchange-stress-load-gen    asia-northeast3-a  e2-standard-8  TERMINATED
goexchange-stress-load-gen-b  asia-northeast3-a  e2-standard-8  TERMINATED
goexchange-stress-server      asia-northeast3-a  e2-highcpu-4   TERMINATED
```

정확히 4대만 정지했고 VM·디스크는 삭제하지 않았다. 시크릿 사본(`token-check.txt` 등)은
4대 모두 잔여 0, `.tmp` 잔여 0으로 확인했다.

---

## 7. 다음

- **500과 750 사이의 실제 경계** — 해상도를 좁히려면 625 VU 등을 10분 × 2회로 측정한다
- **750이 실행마다 갈리는 이유** — r1/r2의 `job_growth` 차이(1.256 vs 1.462) 원인은 미규명
- **남은 비용 성장 항** — 인덱스 제거 후에도 750에서 growth가 1.26~1.46이다. WAL·checkpoint·
  autovacuum·락 경합은 33번에 이어 이번에도 분리하지 않았다
- **B(정확성 부채)** — 취소 command outbox → 주문 idempotency key → 두 matching quantum →
  경로별 DB timeout → `/live`·`/ready` 배선 → shutdown drain. A와 분리된 별도 작업이다
