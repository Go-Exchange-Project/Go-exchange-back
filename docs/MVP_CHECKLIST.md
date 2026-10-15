# MVP Checklist

이 문서는 현재 Go Exchange MVP가 어떤 기능을 지원하는지, 어떤 테스트로 검증했는지, 아직 어떤 리스크가 남아 있는지를 정리합니다.

## 사용자/인증

| 항목 | 상태 | 검증 |
| --- | --- | --- |
| 회원가입 | 완료 | handler/service/unit, E2E register flow |
| 로그인 | 완료 | handler/service/unit, frontend auth flow |
| JWT 발급/검증 | 완료 | auth/middleware tests |
| 보호 API 사용자 스코프 | 완료 | 주문/지갑/체결 조회와 취소 권한 E2E |
| 잘못된 token 처리 | 완료 | `AUTH_INVALID_TOKEN` E2E |
| JWT secret 필수화(폴백 없음) | 완료 | auth env tests |
| 로그인·가입 rate limit(IP 기준) | 완료 | middleware/router tests |

남은 작업:

- refresh token과 token 만료 UX 고도화
- 비밀번호 정책, 계정 잠금

## 주문 정책

| 항목 | 상태 | 검증 |
| --- | --- | --- |
| 지정가 BUY/SELL | 완료 | service tests, E2E order flow |
| 시장가 BUY/SELL | 완료 | matching tests, E2E market order flow |
| 최소 주문금액 | 완료 | policy tests, E2E validation |
| tick size | 완료 | policy tests, E2E validation |
| 코인별 수량 step | 완료 | BTC/ETH/XRP policy tests |
| 거래 중지 market | 완료 | market rules tests, E2E halted market |
| decimal string 계약 | 완료 | service/frontend tests |

남은 작업:

- 주문 정책을 DB나 관리자 설정으로 관리
- 실거래소 수준의 더 세밀한 tick/lot policy

## 매칭엔진

| 항목 | 상태 | 검증 |
| --- | --- | --- |
| 심볼별 오더북 | 완료 | matching tests |
| 가격 우선순위 | 완료 | best bid/ask tests |
| 시간 우선순위 | 완료 | FIFO tests |
| 부분 체결 | 완료 | matching/service/E2E |
| 다중 체결 | 완료 | matching tests |
| 시장가 미체결 잔량 non-rest | 완료 | matching/E2E |
| self-trade 방지 | 완료 | matching/E2E |
| snapshot depth 제한 | 완료 | matching/frontend flow |
| 취소 remove | 완료 | matching/service/E2E |
| 턴 스케줄링(quantum) | 완료 | quantum contract/gate tests |
| 방출 용량 예약과 park | 완료 | capacity/slice tests |
| 취소 backpressure 거절 | 완료 | cancel capacity tests |
| 심볼 샤딩 fan-in | 완료 | sharded engine tests |
| 취소 명령 내구 기록(outbox) | 완료 | cancel command worker/integration |

남은 작업:

- 엔진 명령 ack 구조 고도화
- 영구 global event sequence
- 샤드 fan-in 격리(한 샤드의 적체가 다른 샤드를 막지 않게)

## 자산/정산

| 항목 | 상태 | 검증 |
| --- | --- | --- |
| 복식 부기 원장 | 완료 | ledger service/integration tests |
| available/locked 계정 분리 | 완료 | service tests |
| 수수료 수익 계정(FEE_INCOME) | 완료 | settlement ledger integration |
| 원장 검산 worker | 완료 | reconciliation tests (분개 불균형·캐시 drift·자산 총합·음수 계정) |
| 입출금(가짜 BANK/CHAIN rail) | 완료 | transfer integration tests |
| 입출금 콜백·폴링 이중 확인 | 완료 | transfer/poller tests |
| 일시적 실패 자동 재시도 | 완료 | settlement retry worker tests |
| 주문 hold | 완료 | integration tests |
| 취소 release | 완료 | integration/E2E |
| 체결 settlement | 완료 | integration/E2E |
| 수수료 KRW 부과 | 완료 | settlement/E2E |
| 가격 개선 환급 | 완료 | integration/E2E |
| 평균매수가 | 완료 | service/E2E |
| 중복 정산 방지 | 완료 | idempotency tests |
| settlement 실패 기록 | 완료 | service/integration tests |
| 분개 기록 | 완료 | dev fund, hold, release, settlement, transfer tests |

남은 작업:

- 수수료 정산 보고서와 회계 리포트
- 실제 은행·체인 연동(현재는 가짜 rail)
- `FEE_INCOME` 단일 핫 계정의 경합 측정과 분산

## API/프론트 연동

| 항목 | 상태 | 검증 |
| --- | --- | --- |
| 주문 생성/취소 API | 완료 | handler/service/E2E |
| 주문/지갑/체결 조회 API | 완료 | E2E |
| 입출금 API | 완료 | handler/service/integration tests |
| 주문 멱등성 계약(`Idempotency-Key`) | 완료 | handler/service/integration tests |
| 이체 멱등성 계약(`client_request_key`) | 완료 | transfer tests, 프런트 component tests |
| rate limit `429` + `Retry-After` | 완료 | middleware/router tests |
| structured error response | 완료 | HTTP API tests, E2E |
| market rules API | 완료 | handler/frontend/E2E |
| dev wallet fund API | 완료 | dev token guard tests |
| 프론트 주문 폼 | 완료 | component tests, E2E |
| 계정 패널 | 완료 | component tests, E2E |
| WebSocket reconnect | 완료 | frontend unit tests |

남은 작업:

- 사용자별 실시간 주문/체결 notification stream
- 더 세밀한 API pagination/filter
- 접근성/모바일 UX 추가 점검

## 운영/배포 준비도

| 항목 | 상태 | 검증 |
| --- | --- | --- |
| env 기반 설정 | 완료 | config tests |
| DB credential 하드코딩 제거 | 완료 | config tests |
| CORS/WS origin whitelist | 완료 | config/ws tests |
| goose migration | 완료 | migration tests |
| CI backend test | 완료 | GitHub Actions workflow |
| E2E suite | 완료 | Playwright 21 tests |
| graceful shutdown | 완료 | 종료 순서·분기 테스트(H8·H9) |
| Prometheus metrics / Grafana | 완료 | metrics tests, `monitoring/` 구성 |
| 부하 테스트와 성능 리포트 | 완료 | `docs/benchmarks/` 36건 |
| deployment manifest | 완료 | compose prod/deploy/stress, GHCR 이미지 발행 |
| HTTP·DB 시간 상한 | 완료 | H1~H5, DB 프로필 통합 테스트 |
| JWT secret 필수화 | 완료 | auth env tests |
| rate limit·프록시 신뢰 | 완료 | middleware/router/config tests |
| `/metrics`·pprof 관리 포트 분리 | 완료 | admin server tests |

남은 작업:

- readiness/liveness endpoint 분리
- structured logging과 분산 추적
- SLO·알림 정의
- refresh token과 계정 잠금
- DB pool 획득 상한(`ctx` 전 계층 배선)
- 운영 상한 적용 후 실부하 재측정(현재 측정 환경 없음)

## MVP 결론

현재 MVP는 로컬 개발/시연 기준으로 다음을 보여줄 수 있습니다.

- 두 사용자가 주문을 내고 실제 매칭/정산되는 흐름
- 주문 전 자산 hold와 취소 시 release
- 체결 후 수수료, 평균매수가, 체결 내역 반영
- self-trade, market order, 가격 개선, 최소 주문 정책 같은 거래소 도메인 예외 처리
- DB transaction, idempotency, failed settlement, 복식 부기 원장 기반의 정합성 방어
- 가짜 은행·체인 rail을 통한 입출금 상태 전이와 결과 확인 이중화
- 시간 상한·요청 상한·관리 포트 분리 같은 운영 경계

다만 운영 거래소라고 말하기에는 아직 실제 입출금 연동, 보안 심화(refresh token·계정 잠금), global event stream, SLO·알림이 부족합니다.
