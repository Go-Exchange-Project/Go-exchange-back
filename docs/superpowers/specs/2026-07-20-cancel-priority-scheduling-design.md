# ③ 취소 우선 스케줄링 설계 (2차 리팩토링 · 가용성)

- **날짜**: 2026-07-20
- **상태**: 설계 검토 중
- **로드맵**: [2차 리팩토링 ③](../../refactor/README.md) — 가용성 100%의 셋째 조각
- **근거**: [22번 벤치마크](../../benchmarks/22-2026-07-18-outbox-ab-and-spike.md) — 급등 스파이크에서 취소 시도의 **43.9%(7,959건)가 500 실패**(`CancelCh` 만석 → 1초 타임아웃). 정합성이 아니라 가용성 이슈.

## 왜 필요한가

`MatchingEngine.Start`(engine.go:140-160)의 엔진 루프는 `OrderCh`·`CancelCh`·ticker·
stopCh를 **동등 우선순위 `select`**로 처리한다. Go의 `select`는 준비된 case 중 **무작위**
선택이므로, 주문 부하가 높으면 `OrderCh`가 항상 준비 상태가 되어 취소는 둘 다 준비됐을
때 ~50%만 처리된다 → 취소 지연 → `CancelCh` 만석 → `OrderService.CancelOrder`의 1초
send-timeout 발동 → 500.

취소 실패는 두 원인으로 나뉜다:
- **P1 — select 불공정**: 엔진이 멈춰있지 않아도 동등 `select`라 취소가 주문과 50:50
  경쟁 → 지속 주문 부하에서 취소가 굶는다.
- **P2 — ExecutionCh 정지**: 엔진이 `emitTrade`의 블로킹 `ExecutionCh` 송신에서 멈추면
  (DB가 뒤처짐) `select` 자체가 안 돌아 취소가 아예 처리되지 않는다.

**③은 P1을 구조적으로 없앤다.** P2(엔진 정지)는 ②(DB 천장 상향으로 `ExecutionCh`가 덜
참)·①(주문 게이트로 부하 경감)·④(목표 초과 거절)의 몫이다. ②가 엔진을 안 멈추게 하고
③이 취소에 우선권을 주면, 둘이 합쳐져 목표 부하에서 "취소는 신규 주문에 굶지 않는다"가
성립한다.

## 제약이 설계를 결정한다

오더북은 **단일 writer**(엔진 goroutine이 락 없이 소유)라 취소(`RemoveOrder`)는 **반드시
엔진 goroutine에서** 실행돼야 한다 — 별도 스레드로 분리 불가. 따라서 "취소를 다른 경로로
빼기"는 성립하지 않고, **goroutine 내 우선순위(priority select)** 가 유일한 구조적 방법이다.

## 설계

`MatchingEngine.Start`의 select를 2단계 priority select로 교체한다:

```go
for {
    // 취소 우선: 대기 중 취소를 먼저 논블로킹으로 전부 드레인.
    select {
    case cmd := <-me.CancelCh:
        me.processCancel(cmd)
        continue // CancelCh를 다시 확인 — 취소를 전부 비운 뒤에야 주문을 본다
    default:
    }
    // 취소가 없을 때만 주문/ticker/stop.
    select {
    case cmd := <-me.CancelCh:
        me.processCancel(cmd)
    case order := <-me.OrderCh:
        me.processOrder(order)
    case <-ticker.C:
        me.flushSnapshots()
    case <-me.stopCh:
        me.drainPendingWork()
        me.flushSnapshots()
        if me.ExecutionCh != nil {
            close(me.ExecutionCh)
        }
        close(me.SnapshotCh)
        return
    }
}
```

- 첫 `select`의 `default`는 취소가 없으면 즉시 빠져나와 두 번째 `select`로 — 취소가
  없을 때 바쁜 대기(busy-loop)를 만들지 않는다(두 번째 `select`가 블로킹).
- `continue`가 취소를 **전부 드레인한 뒤에야** 주문을 보게 한다.
- 두 번째 `select`에도 `CancelCh` case를 남긴다: 첫 `select`의 `default` 직후 취소가
  도착하면 두 번째 `select`에서 잡아 즉시 처리(주문과 무작위 경쟁이 아니라, 최소한
  동등 기회 — 이미 첫 단계에서 우선권을 줬으므로 잔여 레이스만 커버).

### 변경 범위 (외과수술적)

- **`MatchingEngine.Start`의 select 한 곳만** 교체. 다른 로직(processOrder/processCancel/
  drainPendingWork/emit*/shutdown 도미노) 전부 무변경.
- `CancelCh` 버퍼(1024)·`OrderService.CancelOrder`의 1초 send-timeout **무변경** —
  priority select가 취소를 빠르게 드레인해 채널이 거의 안 차므로, 기존 타임아웃은
  극단 상황(P2)의 안전 밸브로 그대로 둔다.
- **ShardedEngine 무수정** — 각 샤드가 이 `MatchingEngine`을 쓰므로 자동 커버.

## 엣지 케이스 (전부 감수 가능)

- **취소 폭주가 신규 주문을 굶길 이론적 가능성**: 취소를 전부 드레인하므로 취소가
  끊임없이 오면 주문이 밀린다. 취소는 리스크 관리 행동이라 우선하는 것이 옳고, "취소
  폭주"는 자기 주문만 취소 가능하므로 비현실적 공격이다. 감수한다(사용자 확정).
- **ticker(스냅샷 flush) 지연**: 취소 버스트 중엔 취소를 먼저 드레인하니 스냅샷 flush가
  살짝 밀릴 수 있으나, 스냅샷은 100ms 코얼레싱 + best-effort라 무해.
- **shutdown**: `drainPendingWork`가 이미 `OrderCh`·`CancelCh` 둘 다 드레인 → 종료 시
  순서 무관, 무변경.

## 검토한 대안

- **취소를 별도 경로/goroutine으로 분리**: 오더북 단일 writer 불변 위반(발산·데이터
  레이스). 불가. 기각.
- **`CancelCh` 버퍼만 확대**: 굶주림(P1)을 해소하지 못하고 지연만 미룬다. priority가
  근본. 기각(단독으로는).
- **`CancelOrder` 1초 타임아웃 확대/제거**: 타임아웃은 P2(엔진 정지) 안전 밸브이고, P1은
  priority가 없앤다. 타임아웃 변경은 ③ 범위 밖(P2는 ①②④). 무변경.

## 검증 계획

1. **결정론적 우선순위 증명**: 오더북에 취소 대상 주문 M건을 시드하고, `CancelCh`에 취소
   M건 + `OrderCh`에 신규(체결 유발) 주문 N건을 **버퍼에 미리 채운 뒤** 엔진 기동 →
   `ExecutionCh`에서 나오는 이벤트 순서를 관찰해 **취소(`OrderCancelled`) M건이 신규
   주문발 이벤트(`Trade` 등)보다 먼저** 나오는지 단언. 동등 `select`였다면 섞여 나왔을
   것이므로 우선순위를 실증.
2. 기존 엔진 단위·동시성 테스트 무수정 그린(processOrder/processCancel/shutdown 도미노
   불변의 증거) + `-race`.
3. ShardedEngine 취소 라우팅 테스트 무영향 확인.
4. **취소 실패율 실증은 ⑤(23번)** — ③은 실패율 수치를 주장하지 않는다. 성공 기준은
   "취소가 신규 주문보다 먼저 처리됨을 결정론적으로 증명 + 회귀 그린"까지.

## 범위 밖

- P2(엔진 `ExecutionCh` 정지) — ①②④의 몫.
- ④(입장 정책 정교화), ⑤(23번 실증 — 스파이크에서 취소 실패율 0 확인).
- API 계약 변경 없음(`DELETE /orders/:id`의 요청/응답 불변).
