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

남은 작업:

- refresh token과 token 만료 UX 고도화
- 비밀번호 정책, rate limit, 계정 잠금

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

남은 작업:

- 엔진 명령 ack 구조 고도화
- 영구 global event sequence
- crash/replay를 위한 event sourcing 또는 durable command log 검토

## 자산/정산

| 항목 | 상태 | 검증 |
| --- | --- | --- |
| available/locked wallet | 완료 | service tests |
| 주문 hold | 완료 | integration tests |
| 취소 release | 완료 | integration/E2E |
| 체결 settlement | 완료 | integration/E2E |
| 수수료 KRW 부과 | 완료 | settlement/E2E |
| 가격 개선 환급 | 완료 | integration/E2E |
| 평균매수가 | 완료 | service/E2E |
| 중복 정산 방지 | 완료 | idempotency tests |
| settlement 실패 기록 | 완료 | service/integration tests |
| ledger entries | 완료 | dev fund, hold, release, settlement tests |

남은 작업:

- exchange-level double-entry accounting
- 자동 reconciliation/retry worker
- fee revenue account와 수수료 정산 보고서

## API/프론트 연동

| 항목 | 상태 | 검증 |
| --- | --- | --- |
| 주문 생성/취소 API | 완료 | handler/service/E2E |
| 주문/지갑/체결 조회 API | 완료 | E2E |
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
| E2E suite | 완료 | Playwright 16 tests |

남은 작업:

- deployment manifest
- readiness/liveness endpoint 분리
- graceful shutdown
- structured logging
- Prometheus metrics/Grafana dashboard
- 부하 테스트와 성능 개선 리포트

## MVP 결론

현재 MVP는 로컬 개발/시연 기준으로 다음을 보여줄 수 있습니다.

- 두 사용자가 주문을 내고 실제 매칭/정산되는 흐름
- 주문 전 자산 hold와 취소 시 release
- 체결 후 수수료, 평균매수가, 체결 내역 반영
- self-trade, market order, 가격 개선, 최소 주문 정책 같은 거래소 도메인 예외 처리
- DB transaction, idempotency, failed settlement, ledger 기반의 정합성 방어

다만 운영 거래소라고 말하기에는 아직 배포, 모니터링, 실입출금, 보안 심화, global event stream이 부족합니다.
