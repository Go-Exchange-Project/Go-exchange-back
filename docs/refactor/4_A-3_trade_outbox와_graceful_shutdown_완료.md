# 4. A-3 trade outbox + graceful shutdown — 완료

- **완료일**: 2026-07-12
- **상세 설계·검증**: [docs/superpowers/specs/2026-07-12-trade-outbox-graceful-shutdown-design.md](../superpowers/specs/2026-07-12-trade-outbox-graceful-shutdown-design.md)

## 왜 문제였나

체결(trade)과 시장가 완료(MarketOrderDone) 이벤트가 엔진 → 정산 사이에서
**인메모리 채널에만** 존재했다. 크래시·OOM·배포 재시작 때마다 채널과 정산 큐에
있던 이벤트가 증발했다: trade 유실 = 매수자 코인·매도자 KRW 미지급, Done 유실 =
시장가 잔여 hold 영구 동결. A-1/A-2가 "정산이 실패하는" 경로를 닫은 뒤 남은
**마지막 정합성 구멍**이었고, 배포할 때마다 유실 창이 열렸다.

## 어떻게 해결했나

**핵심 불변식: "정산은 outbox에 커밋된 이벤트만 처리한다" (write-ahead).**
이 불변식이 서면 크래시 타이밍이 어디든 정합성이 유지된다 — 커밋 전 크래시는
매칭 자체가 롤백(돈 무변동, 부트스트랩이 미체결 주문 재투입), 커밋 후 크래시는
부팅 리플레이가 완결(멱등성 키가 이중 정산 차단 → exactly-once 효과).

- **OutboxWriter**: ExecutionCh의 유일한 소비자. 이벤트를 64건/5ms 배치로 한
  트랜잭션에 커밋(group commit — B-4의 선행 형태)한 뒤에만 심볼 파티셔닝 큐로
  전달. INSERT 실패는 무한 재시도하며 이 동안 엔진이 블록되는 건 의도된
  백프레셔(DB가 죽었는데 매칭만 계속되면 유실 대기 이벤트가 무한 적체).
- **부팅 리플레이**: PENDING 이벤트를 ID 순 순차 재처리(심볼별 FIFO 자명 보존).
  내구 확정 실패는 PENDING 유지(다음 부팅 재시도), 손상 행은 격리.
- **시장가 파이널라이저**: 리플레이 직후, DB에 PENDING/PARTIAL로 남은 시장가는
  엔진 메모리가 사라져 더 체결될 수 없으므로 잔여 hold를 해제하고 상태 확정.
  리컨실리에이션 검사 3이 탐지만 하던 영구 동결 케이스의 **자동 복구**.
- **graceful shutdown**: SIGTERM → HTTP 차단 → 엔진 드레인 후 채널 close →
  writer 잔여 flush 후 큐 close → 워커 드레인 — close가 도미노로 전파되는
  종료 체인. 상한(30s) 초과로 강제 종료돼도 outbox 덕에 유실이 아니다.

## 결과 (전부 실측)

- **결정론적 크래시 주입** (실제 바이너리): "outbox 커밋 직후·정산 직전" 상태를
  주입하고 부팅 → 체결 정확히 1회 정산(수수료 50원까지 정확), Done 잃은 시장가
  CANCELLED 확정 + locked 50,000 전액 해제.
- **무작위 SIGKILL 3회** (체결 버스트 도중): 매회 재기동 후 원장-지갑 gap 0,
  PENDING 0, 미해소 주문 0 — 불변식이 어느 타이밍에도 유지.
- **graceful shutdown** (버스트 도중 SIGTERM): exit 0, 체결 30/30 전부 정산,
  유실 0 — **배포 재시작의 유실 창이 닫혔다.**
- 단위/통합 테스트 ~25건 추가, 전체 스위트 그린.
- 이로써 **크래시 시나리오를 포함한 "정합성 100%"를 주장할 수 있게 됐다**:
  정산 실패 복구(A-1/A-2) + 상시 검증(A-6) + 백업(C-2) + 크래시 내구성(A-3).
- 부수 효과: 엔진이 정산 속도에 결박되던 문제(B-2)가 outbox 경유로 분리됐고,
  group commit 인프라가 B-4의 토대가 된다.

## k6 전후 비교 결과 (2026-07-13 실측)

[docs/benchmarks/17-2026-07-13-trade-outbox-throughput-impact.md](../benchmarks/17-2026-07-13-trade-outbox-throughput-impact.md)
— 같은 VM 연속 A/B(before=`f2ec22a`, after=`37a3e49`), 램프업·hold 두 프로파일:

| 프로파일 | 처리량 변화 | p95 변화 | 실패율 |
|---|---|---|---|
| 램프업(VU 50→3000) | 154.8→128.9 iter/s (**−16.7%**) | 14.88→18.66s | 0% |
| hold(VU 3000 8분) | 162.7→133.8 iter/s (**−17.8%**) | 16.7→21.07s | 0% |

두 프로파일 일관 ~17% 감소 = 노이즈 아닌 실제 비용. **원인은 outbox INSERT가
아니라 MarkProcessed UPDATE**: INSERT는 991배치로 뭉쳐(44.7건/배치) 왕복이 적은
반면, MarkProcessed는 trade당 개별 UPDATE 44,300회. 정합성(유실 0)의 대가지만
대부분 회수 가능한 종류다.

## 남은 것

- **MarkProcessed를 정산 트랜잭션에 흡수**(왕복 2회→1회) — 위 벤치마크가 지목한
  최적화. B-4(정산 그룹커밋)와 함께 다루면 자연스럽다. 백로그 등록됨.
- PROCESSED 행 보존 정리(아카이빙) — 백로그.
