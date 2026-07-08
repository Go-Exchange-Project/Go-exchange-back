# 4번째 테스트 (2026-07-08): 매칭엔진 CPU 프로파일링 (pprof)

## 커밋 해시

`3122c33` (fix: pprof 서버 바인드 주소를 :6060(모든 인터페이스)으로 변경)

## 왜 이 조사를 했는지

3번째 테스트(`03-2026-07-08-gcp-stress-test.md`)에서 VU 150~200 구간부터 CPU가 93%까지 포화되며 처리량이 정체됐다. 이때 `order_pipeline_match_latency_seconds`(매칭엔진 큐잉+처리 지연) p95가 10초까지 치솟았는데, 이 지표는 "채널에서 대기한 시간"과 "실제 매칭 연산에 걸린 시간"을 구분하지 않는다. 매칭 로직 자체가 느려진 것인지(가설 A), 아니면 매칭 로직은 여전히 빠른데 다른 요인(CPU 스케줄링 경쟁, 주문마다 실행되는 오더북 스냅샷 생성 등)이 매칭 루프를 무겁게 만든 것인지(가설 B) 추측이 아니라 근거로 확인하기 위해, 실제 CPU 프로파일을 캡처했다.

## 왜 pprof를 선택했는지

파이프라인 지표를 더 세분화(`order_engine_queue_wait_duration` vs `order_engine_match_duration` 분리)하는 방법도 검토했지만, 그러려면 원인을 미리 짐작해서 계측 지점을 정확히 골라야 한다. pprof는 원인을 모르는 상태에서도 "실제로 CPU 시간이 어느 함수에서 소비됐는지"를 통째로 보여주므로, 가설 공간을 좁히는 더 빠른 방법이었다. (설계 근거: `docs/superpowers/specs/2026-07-08-pprof-cpu-profiling-design.md`)

## 실행한 정확한 절차

1. `GOEXCHANGE_ENABLE_PPROF=true`로 서버 인스턴스에 재배포 (`docker-compose.stress.yml`)
2. SSH 터널: `ssh -L 6060:localhost:6060 -i ~/.ssh/goexchange-gcp goexchange@8.230.15.177`
3. k6 스트레스 테스트(`order-submission-stress.js`) 재실행
4. VU 147, CPU 93.6% 시점(3번째 테스트와 동일한 병목 구간)에서:
   ```bash
   go tool pprof -seconds=30 -output=cpu.prof http://localhost:6060/debug/pprof/profile
   ```

## 진행 중 발견하고 고친 버그 1건

**pprof 서버가 컨테이너 안에서 `127.0.0.1:6060`에 바인딩되어 있어 docker-compose의 호스트측 포트 매핑(`127.0.0.1:6060:6060`)이 전혀 도달하지 못하는 문제**를 실제 배포 중 발견했다. Docker의 포트 포워딩은 컨테이너의 `eth0`을 거쳐 들어오는데, 앱이 컨테이너 자신의 루프백에만 붙어 있으면 그 트래픽을 받을 수 없다. "외부 노출 차단"의 역할을 앱 바인드 주소가 아니라 docker-compose의 호스트측 `127.0.0.1` 매핑 하나로만 담당하도록 고쳤다(`:6060`으로 변경, 커밋 `3122c33`). 보안 성격(GCP 바깥에서는 절대 접근 불가, SSH 터널로만 접근)은 그대로 유지된다.

## 원본 출력

### 프로파일 요약

```
File: go-exchange-back
Build ID: 26af040c3adc78998f5ed86531d5c84d00a441eb
Type: cpu
Time: 2026-07-08 21:59:02 KST
Duration: 30.22s, Total samples = 8.10s (26.80%)
```

### `go tool pprof -top` (flat 기준 상위, 원본 출력 그대로)

```
      flat  flat%   sum%        cum   cum%
     2.25s 27.78% 27.78%      2.25s 27.78%  crypto/internal/fips140/sha256.blockAVX2
     1.32s 16.30% 44.07%      1.32s 16.30%  internal/runtime/syscall.Syscall6
     0.15s  1.85% 45.93%      2.49s 30.74%  crypto/internal/fips140/sha256.(*Digest).Write
     0.15s  1.85% 47.78%      2.58s 31.85%  crypto/internal/fips140/sha256.(*Digest).checkSum
     0.15s  1.85% 49.63%      0.15s  1.85%  runtime.futex
     0.15s  1.85% 51.48%      0.15s  1.85%  runtime.memmove
     0.12s  1.48% 52.96%      3.07s 37.90%  crypto/internal/fips140/pbkdf2.Key[...]
     0.10s  1.23% 54.20%      0.27s  3.33%  runtime.scanobject
     0.09s  1.11% 55.31%      0.09s  1.11%  runtime.nextFreeFast (inline)
     0.08s  0.99% 56.30%      2.72s 33.58%  crypto/internal/fips140/sha256.(*Digest).Sum
     0.08s  0.99% 57.28%      0.11s  1.36%  runtime.findObject
     0.08s  0.99% 58.27%      0.08s  0.99%  runtime.memclrNoHeapPointers
     0.07s  0.86% 59.14%      0.31s  3.83%  runtime.newobject
     0.06s  0.74% 59.88%      0.06s  0.74%  aeshashbody
     0.05s  0.62% 61.98%      2.88s 35.56%  crypto/internal/fips140/hmac.(*HMAC).Sum
     0.05s  0.62% 63.21%      0.60s  7.41%  runtime.mallocgc
     0.04s  0.49% 64.32%      2.29s 28.27%  crypto/internal/fips140/sha256.block
     0.03s  0.37% 66.17%      0.34s  4.20%  github.com/jackc/pgx/v5/pgproto3.(*Frontend).Receive
     0.02s  0.25% 67.04%      0.51s  6.30%  database/sql.(*DB).execDC
     0.02s  0.25% 67.78%      3.52s 43.46%  gorm.io/gorm.(*DB).Begin
     0.01s  0.12% 69.88%      0.98s 12.10%  database/sql.(*DB).queryDC
     0.01s  0.12% 70.25%      1.66s 20.49%  database/sql.withLock
     0.01s  0.12% 70.49%      0.25s  3.09%  internal/matching.(*MatchingEngine).GetOrderBookSnapshotWithDepth.func2
     0.01s  0.12% 71.85%      6.18s 76.30%  gorm.io/gorm.(*DB).Transaction
     0.01s  0.12% 72.35%      2.33s 28.77%  gorm.io/gorm/callbacks.(*processor).Execute
     0.01s  0.12% 73.09%      4.97s 61.36%  net/http.(*conn).serve
```

(전체 출력은 `go tool pprof -top`으로 재현 가능; 위는 대표 라인만 발췌)

### 콜스택 추적: `pbkdf2.Key`의 호출자 (`go tool pprof -peek=pbkdf2.Key`)

```
                                             3.07s   100% |   github.com/jackc/pgx/v5/pgconn.(*scramClient).clientFinalMessage
         0     0%  1.48%      3.07s 37.90%                | crypto/pbkdf2.Key[...]
                                             3.07s   100% |   crypto/internal/fips140/pbkdf2.Key[...]
```

### 콜스택 추적: 누적(cum) 기준 상위 (`go tool pprof -top -cum`)

```
      flat  flat%   sum%        cum   cum%
     0.01s  0.12%  0.12%      6.18s 76.30%  gorm.io/gorm.(*DB).Transaction
     0.01s  0.12%  0.25%      4.97s 61.36%  net/http.(*conn).serve
         0     0%  0.25%      4.75s 58.64%  github.com/gin-gonic/gin.(*Engine).ServeHTTP
         0     0%  0.25%      4.66s 57.53%  internal/handler.(*OrderHandler).CreateOrder
         0     0%  0.25%      4.63s 57.16%  internal/service.(*OrderService).CreateOrder
     0.02s  0.25%  0.49%      3.52s 43.46%  gorm.io/gorm.(*DB).Begin
         0     0%  0.49%      3.49s 43.09%  database/sql.(*DB).BeginTx
         0     0%  0.49%      3.40s 41.98%  database/sql.(*DB).conn
         0     0%  0.49%      3.39s 41.85%  github.com/jackc/pgx/v5/stdlib.connector.Connect
         0     0%  0.49%      3.38s 41.73%  github.com/jackc/pgx/v5.ConnectConfig
         0     0%  0.49%      3.32s 40.99%  github.com/jackc/pgx/v5/pgconn.connectOne
         0     0%  0.49%      3.16s 39.01%  github.com/jackc/pgx/v5/pgconn.(*PgConn).scramAuth
         0     0%  0.49%      3.08s 38.02%  github.com/jackc/pgx/v5/pgconn.(*scramClient).clientFinalMessage
         0     0%  0.49%      3.07s 37.90%  crypto/pbkdf2.Key[...]
```

## 요약 테이블

| 함수/구간 | 누적(cum) CPU 비중 | 해석 |
|---|---|---|
| `gorm.(*DB).Transaction` (전체 DB 트랜잭션) | 76.30% | `CreateOrder` 경로의 대부분이 여기 |
| `gorm.(*DB).Begin` → `database/sql.(*DB).conn` (커넥션 획득) | 43.46% / 41.98% | 트랜잭션 시작 = 새 커넥션 확보 비용이 대부분 |
| `pgx.ConnectConfig` → `scramAuth` → `clientFinalMessage` → `pbkdf2.Key` (신규 커넥션의 SCRAM-SHA-256 인증) | **37.90%** | **CPU의 3분의 1 이상이 매 요청마다 새 DB 커넥션을 맺는 인증 과정에서 소모됨** |
| `internal/matching.*` (매칭엔진 전체: Start 루프, 스냅샷 생성, BTree 순회) | 3.83% + 3.09% + 3.09% ≈ **10% 미만** | 매칭 로직 자체는 전혀 무겁지 않음 |
| `net/http` + `gin` (HTTP 처리 자체) | 58.64%~61.36% (트랜잭션 처리와 겹치는 구간 포함) | HTTP 계층 자체의 오버헤드는 크지 않음 |

## 해석 (가설 A vs 가설 B, 그리고 예상 밖의 결과)

1. **가설 A(매칭 로직 자체가 느리다)는 명확히 기각된다.** 매칭엔진 전체(매칭 루프, 오더북 스냅샷 생성, BTree 순회 포함)가 차지하는 CPU 비중은 10% 미만이다. 3번째 테스트에서 관측했던 `order_pipeline_match_latency_seconds` p95 10초는 매칭 연산 자체 때문이 아니었다.
2. **가설 B(스냅샷 생성이 매칭 루프를 무겁게 만든다)도 기각된다.** `GetOrderBookSnapshotWithDepth`는 누적 3.09%에 불과해, 브레인스토밍 때 의심했던 만큼 무겁지 않았다.
3. **예상하지 못했던 진짜 원인: DB 커넥션 풀이 전혀 설정되어 있지 않다.** `config/database.go`의 `ConnectDB()`는 `gorm.Open(...)`만 호출하고 `SetMaxOpenConns`/`SetMaxIdleConns`/`SetConnMaxLifetime`을 전혀 설정하지 않는다. Go `database/sql`의 기본값(유휴 커넥션 2개, 열린 커넥션 수 무제한)에서는, 동시 요청이 유휴 커넥션 수를 넘어서는 순간마다 **새 TCP 연결 + Postgres 인증(SCRAM-SHA-256, PBKDF2 키 유도 포함)을 처음부터 다시** 해야 한다. 이 프로파일에서는 그 인증 과정 하나(`pbkdf2.Key`)가 전체 CPU 샘플의 **37.90%**를 차지했다 — 매칭엔진 전체보다 4배 가까이 큰 비중이다.
4. **결론: 이번 스트레스 테스트의 CPU 포화는 매칭엔진이 아니라, 설정되지 않은 DB 커넥션 풀로 인한 반복적인 커넥션 재수립(및 그때마다의 SCRAM 인증) 비용이 지배적 원인이다.** 이는 브레인스토밍 단계의 가설(매칭엔진 우선순위)과 다른 결과이며, pprof로 실측하지 않았다면 매칭엔진 쪽을 잘못 최적화할 뻔했다는 점에서 이 조사의 가치가 크다.

## 다음 작업 제안 (우선순위순, 이번 스코프 밖 — 구현은 별도 작업)

1. **(최우선) DB 커넥션 풀 설정 추가** — `config/database.go`의 `ConnectDB()`에서 `sqlDB.SetMaxOpenConns(N)`, `SetMaxIdleConns(N)`, `SetConnMaxLifetime(...)`을 명시적으로 설정한다. 목적: 유휴 커넥션을 늘려서 매 요청마다 SCRAM 인증을 반복하지 않도록 한다. 기대 효과: CPU의 ~38%를 차지하던 인증 비용이 대부분 사라지고, 같은 VU 구간에서 CPU 사용률과 p95 지연이 크게 낮아질 것으로 예상된다(정확한 수치는 재측정 필요). 검증 방법: 같은 스트레스 테스트를 동일 VU 프로파일로 재실행하고, (a) `docs/benchmarks/03-...`의 CPU/p95 수치와 비교, (b) 같은 방식으로 pprof를 다시 떠서 `pbkdf2.Key`/`scramAuth` 비중이 실제로 줄었는지 확인한다.
2. **매칭엔진 관련 개선(심볼별 샤딩 등)은 이번 근거로는 우선순위가 낮다.** 매칭엔진 자체가 CPU의 10% 미만만 쓰고 있으므로, 커넥션 풀 문제를 먼저 해결한 뒤에도 여전히 매칭엔진이 병목이라면 그때 다시 검토한다.
3. **k6 setup/create_order 지표 분리**는 이번 조사와 독립적인 위생 작업으로 계속 보류.

## 범위 밖 (Out of Scope)

- 위에서 제안한 DB 커넥션 풀 설정 변경의 실제 구현 및 재측정 — 별도 브레인스토밍/계획으로 진행.
- 매칭엔진 샤딩 여부 결정 — 커넥션 풀 수정 후 재평가.
