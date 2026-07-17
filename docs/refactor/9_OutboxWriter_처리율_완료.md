# 9. OutboxWriter 처리율 개선 — 완료

- **완료일**: 2026-07-17
- **상세 설계**: [OutboxWriter 처리율 개선 설계](../superpowers/specs/2026-07-17-outbox-writer-throughput-design.md)

## 왜 문제였나

[21번 벤치마크](../benchmarks/21-2026-07-17-b3-sharding-remeasurement.md)의 실측:
outbox flush가 배치 상한 64에 포화(평균 54.4건/flush)된 채 초당 15.2회
(≈66ms/flush) 돌아, write-ahead 관문의 처리율이 **≈827 events/s로 캡**돼 있었다.
모든 체결이 이 관문을 통과하므로(A-3 불변식) 이 캡이 곧 전체 파이프라인 캡이고,
execution 채널 만석(8192)과 order 채널 만석 — 20번에서 엔진 직렬화로 오진했던
적체 — 의 근원이었다.

## 어떻게 해결했나

**로직 무변경, 상한만 상향.** 수집 루프·무한 재시도 백프레셔·forward 순서·graceful
shutdown 전부 그대로 두고:

- `defaultOutboxBatchSize` 64 → **512**, `GOEXCHANGE_OUTBOX_BATCH_SIZE` 환경변수로
  조정 가능(왕복·fsync 횟수 1/8, 파라미터 ~3.6k « Postgres 한도 65,535).
- FlushInterval(5ms) 유지 — 저부하 지연 특성 불변, 포화 시에만 배치가 커지는
  적응 패턴(B-4·B-1c와 동일).
- `trade_outbox_flush_batch_size` 버킷을 1024까지 확장(포화 여부가 이 작업의
  판정 수단).
- 정산 큐(256)는 키우지 않았다 — 21번 교훈(병목이 다운스트림일 때 깊은 버퍼는
  꼬리 지연만 키운다). forward 블록은 의도된 백프레셔다.
- **라이더**: 기본 엔진 샤드 수 NumCPU → 1 (21번 실측: 병목이 다운스트림일 때
  샤드 N = 주문 버퍼 N×1024로 p95만 +57% 악화. env로 필요 시 확대).
- 병렬 writer(심볼 파티션)는 보류 — 승격 조건은 22번 측정에서 "새 상한에도
  flush 포화 + execution 채널 만석"(설계 문서의 대안 절 참조).

## 결과

- 신규 테스트: env 파싱 3케이스 + 대배치(512 상한 채움 + 잔여 배치, 순서 보존)
  1건. **기존 outbox writer·리플레이·크래시 계약 테스트는 무수정 그린**(로직
  무변경의 증거). 전체 스위트(통합 포함, SKIP 0) + `-race` 그린.
- **처리량 수치는 이 문서에서 주장하지 않는다** — 실증은 22번 측정 사이클.
  같은 바이너리로 env만 바꾸는 A/B(64 vs 512, 필요 시 128/256/1024 스위프)라
  재빌드·배포 변수까지 제거된 비교가 가능하다. 판정: ① flush 포화 해제
  ② execution 채널 만석 해제 ③ 처리량 상승 폭 ④ 다음 병목(DB CPU 258% 예상) 식별.

## 남은 것

- **22번 GCP 재측정** — 위 판정 4개.
