# 정산(ExecutionCh) 소비자 워커 풀 도입 설계

## 배경 (왜 필요한가)

`09-2026-07-09-matching-engine-channel-length.md`에서 매칭 지연(p95 14~27초)의 진짜 원인을 실측으로 확인했다: `cmd/main.go`가 `ExecutionCh`를 소비하는 정산 고루틴을 **단 하나만** 띄우고 있어서, 부하가 몰리면 이 고루틴이 체결 이벤트를 처리하는 속도가 유입 속도를 못 따라가고, `ExecutionCh`(버퍼 1024)가 가득 찬다. 매칭엔진의 `Match()`는 이 채널에 동기적으로 전송(`me.ExecutionCh <- ...`)하기 때문에, 버퍼가 차면 매칭 루프 자체가 블로킹되고, 그 뒤로 `OrderCh`(입력 큐)까지 밀린다.

이번 작업은 이 병목을 코드 수준에서 직접 해소한다 — 정산 소비자를 여러 고루틴(워커 풀)으로 늘려서, `ExecutionCh` 소비 속도를 높인다. VM 사양이나 Redis/Kafka 같은 새 인프라 없이, 기존 DB 커넥션 풀(05번 테스트에서 25개로 튜닝)을 더 잘 활용하는 방식이라 로드맵의 "가장 간단한 수정" 단계에 해당한다.

## 왜 이 방식을 선택했는지

`internal/service/settlement_service.go`의 `SettleTrade`를 확인했다 — `s.DB.Transaction(func(tx *gorm.DB) error {...})` 안에서 멱등키(`IdempotencyKey`) 중복 체크와 지갑/주문 갱신을 전부 처리한다. 즉 여러 고루틴이 동시에 `SettleTrade`를 호출해도, Postgres가 트랜잭션과 행 잠금으로 충돌하는 쓰기를 안전하게 직렬화해준다 — 애플리케이션 레벨에서 별도 동기화가 필요 없다. 이 덕분에 정산 고루틴을 여러 개로 늘리는 게 가장 적은 변경으로 가능한 해결책이다.

워커 개수는 환경변수로 설정 가능하게 하고 기본값을 10으로 뒀다 — DB 커넥션 풀 최대치(25)에 여유가 있는 값이라 다른 작업(API 요청 처리 등)과 충돌 없이 동시 정산을 늘릴 수 있다. `config/database.go`의 `parsePositiveIntEnv` 패턴을 그대로 재사용해서, 기존 DB 풀 튜닝 설정들과 일관된 방식으로 설정한다.

## 범위

- `config/runtime.go`에 `SettlementWorkersFromEnv() int` 함수를 추가한다.
- `cmd/main.go`에서 `ExecutionCh`를 소비하는 고루틴을 1개에서 `config.SettlementWorkersFromEnv()`개로 늘린다.
- `docker-compose.stress.yml`/`.env.stress.example`에 `GOEXCHANGE_SETTLEMENT_WORKERS=10`을 추가한다.
- 재배포/재측정/결과 문서화는 이 스펙 문서화 이후 사용자와 직접 진행한다(코드/설정 준비까지만 계획에 담는다).

## 아키텍처

### 1. `config/runtime.go`에 워커 개수 설정 추가

```go
const (
	EnvGOExchangeEnableDevTools    = "GOEXCHANGE_ENABLE_DEV_TOOLS"
	EnvGOExchangeDevToolsToken     = "GOEXCHANGE_DEV_TOOLS_TOKEN"
	EnvGOExchangeEnableUpbit       = "GOEXCHANGE_ENABLE_UPBIT"
	EnvGOExchangeCORSOrigins       = "GOEXCHANGE_CORS_ALLOWED_ORIGINS"
	EnvGOExchangeEnablePprof       = "GOEXCHANGE_ENABLE_PPROF"
	EnvGOExchangeSettlementWorkers = "GOEXCHANGE_SETTLEMENT_WORKERS"
)

const defaultSettlementWorkers = 10

func SettlementWorkersFromEnv() int {
	return parsePositiveIntEnv(EnvGOExchangeSettlementWorkers, defaultSettlementWorkers)
}
```

`parsePositiveIntEnv`는 `config/database.go`에 이미 정의된 패키지 내부 함수(같은 `config` 패키지)를 그대로 재사용한다 — 값이 없거나 파싱 실패/0 이하면 기본값(10)으로 조용히 폴백하는 기존 동작을 그대로 따른다.

### 2. `cmd/main.go`에서 워커 풀로 전환

현재(단일 고루틴):
```go
	go func() {
		for event := range me.ExecutionCh {
			processExecutionEvent(event, settlementService, failedSettlementService, orderService, func(msg []byte) {
				hub.Broadcast <- msg
			}, log.Default())
		}
	}()
```

다음과 같이 워커 개수만큼 동일한 고루틴을 띄우는 방식으로 바꾼다:
```go
	for i := 0; i < config.SettlementWorkersFromEnv(); i++ {
		go func() {
			for event := range me.ExecutionCh {
				processExecutionEvent(event, settlementService, failedSettlementService, orderService, func(msg []byte) {
					hub.Broadcast <- msg
				}, log.Default())
			}
		}()
	}
```

여러 고루틴이 같은 채널을 `range`해도 Go 런타임이 각 항목을 정확히 하나의 고루틴에게만 전달하므로 이중 처리 걱정이 없다.

### 3. 스트레스 환경 설정 추가

`docker-compose.stress.yml`의 `backend` 서비스 `environment`에 추가:
```yaml
      GOEXCHANGE_SETTLEMENT_WORKERS: ${GOEXCHANGE_SETTLEMENT_WORKERS:-10}
```

`.env.stress.example`에 추가:
```
GOEXCHANGE_SETTLEMENT_WORKERS=10
```

### 검증 절차 (사용자와 직접 진행, 이 계획의 범위 밖)

1. 서버 인스턴스에 재배포, `docker compose -f docker-compose.stress.yml up -d --build --force-recreate`로 재기동.
2. k6 스트레스 테스트를 04~09번 테스트와 동일한 조건으로 재실행.
3. `matching_engine_channel_length{channel="execution"}`, `channel="order"`가 이전(09번, 상한까지 포화)과 달리 여유가 생기는지, `order_pipeline_match_latency_seconds` p95가 실제로 줄어드는지 확인.
4. 결과를 `docs/benchmarks/10-YYYY-MM-DD-settlement-worker-pool.md`에 기록 — 09번 문서를 기준선으로 전/후 비교.

## 성공 기준

- 코드가 빌드/기존 테스트(특히 `internal/service`의 정산 통합 테스트)를 통과한다.
- 재측정 후, `execution`/`order` 채널 포화 여부와 매칭 지연 p95가 09번 대비 어떻게 바뀌었는지 있는 그대로 기록된다 — 개선이든 아니든 과장 없이.

## 범위 밖 (Out of Scope)

- Redis Streams 등으로 정산을 진짜 비동기 큐로 옮기는 것 — 워커 풀만으로 부족하면 로드맵의 다음 단계로 검토.
- 워커 개수를 동적으로 조절하는 기능 — 고정 개수(환경변수)로 충분하다.
- VM 사양 변경.
