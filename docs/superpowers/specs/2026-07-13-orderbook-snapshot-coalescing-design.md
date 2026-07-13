# B-1(a): 오더북 스냅샷 코얼레싱 + 캐시로 엔진 분리

- 작성: 2026-07-13 (Fable5 설계·구현)
- 로드맵: [docs/refactor/README.md](../../refactor/README.md) 5번

## 왜 필요한가

매칭 엔진 루프(단일 goroutine)가 매칭 외에 스냅샷 생성·전송·REST 조회까지 겸한다:

```
select {
  case order: Match() + GetOrderBookSnapshot(depth30 순회+decimal) + SnapshotCh<-(블로킹)
  case cancel: 동일
  case SnapshotReq: REST 스냅샷 요청 처리
}
```

세 가지 문제:
1. **주문마다 스냅샷 생성**(depth 30 순회 + decimal 합산)이 매칭 처리량을 직접 갉아먹는다.
   급등락 시 중간 스냅샷은 대부분 버려지는데도 매번 만든다.
2. **SnapshotCh 블로킹 전송** — 소비자(WS 브로드캐스트)가 느리면 매칭이 정지한다
   (9번 벤치마크에서 확인된 엔진 결박 패턴).
3. **REST 스냅샷도 엔진 루프가 처리**(SnapshotReq)해 매칭과 경쟁한다.

목표: 스냅샷 조회·브로드캐스트를 엔진 루프에서 떼어내 매칭을 결박에서 해방한다.

## 설계

### 1. 심볼별 스냅샷 캐시

엔진에 `snapshotCache sync.Map`(key: 심볼, value: `*OrderBookSnapshot`)을 둔다.
엔진이 최신 스냅샷을 통째로 Store하고, 읽는 쪽(REST 핸들러)은 Load한다. 스냅샷은
불변으로 취급(교체만)하므로 락이 필요 없다.

### 2. 엔진 루프에서 스냅샷 생성 제거 → dirty 마킹 + 티커 코얼레싱

- `processOrder`/`processCancel`은 `SnapshotCh <- GetOrderBookSnapshot(...)` 대신
  `markDirty(coinSymbol)`만 한다. dirty 집합은 엔진 goroutine 로컬 `map[string]bool`이라
  락이 없다.
- 엔진 루프에 스냅샷 티커(기본 100ms)를 추가한다. tick마다 `flushSnapshots()`가
  dirty 심볼만 스냅샷을 생성해 ① 캐시에 Store ② `SnapshotCh`에 **논블로킹** 전송
  (`select { case ch<-snap: default: }` — 가득 차면 skip, 어차피 다음 tick에 최신본)
  후 dirty를 비운다.
- 스냅샷 생성은 여전히 엔진 goroutine 안에서만 일어나므로 오더북 자료구조에 대한
  레이스가 없다. 다만 빈도가 "주문마다"에서 "100ms마다 dirty 심볼당 1회"로 급감한다
  (코얼레싱). 100ms에 같은 심볼 주문 1000건이 와도 스냅샷은 1회.

### 3. REST 조회를 캐시로 분리

- `RequestOrderBookSnapshot(coinSymbol, depth)`이 SnapshotReq 채널로 엔진에 요청하는
  대신 캐시에서 Load한다. 캐시에 아직 없으면(첫 tick 전) 빈 스냅샷을 반환한다
  (신규 심볼은 실제로 비어 있고, 첫 tick 후 채워진다).
- 캐시는 `DefaultSnapshotDepth`(30)로 생성되므로, 요청 depth는 30으로 상한 클램프하고
  캐시본을 그 depth로 truncate해 반환한다. depth>30은 오더북 UI에서 실용성이 낮아
  (Upbit도 15~30 수준) 이 제약을 둔다.
- 엔진 루프에서 `SnapshotReq` case와 관련 채널을 제거한다.

### 4. 브로드캐스트 경로

`SnapshotCh` 소비 goroutine(main.go)은 그대로 둔다 — 이제 코얼레싱된 스냅샷만
흐르므로 JSON marshal + hub.Broadcast 부하도 심볼당 최대 10회/초로 준다.

### graceful shutdown

엔진 stop 시 `drainPendingWork()` 후 `flushSnapshots()`를 한 번 더 호출해 마지막
dirty 상태를 반영하고 `SnapshotCh`를 닫는다(기존 close 도미노 유지).

## 스코프 밖 (후속 B-1b)

- **WS 심볼 구독**: 클라이언트가 관심 심볼만 받도록 하는 것은 hub 구조 변경이라
  별도 작업. 이번엔 전체 브로드캐스트를 유지한다.
- 스냅샷 주기 env 노출: 우선 상수(100ms). 필요해지면 후속.

## 검증 방법

1. **단위(matching)**:
   - 코얼레싱: 같은 심볼 주문 N건 + 티커 1회 → 스냅샷 생성/전송 1회.
   - 캐시: 주문 → flush → `RequestOrderBookSnapshot`이 캐시에서 최신 스냅샷 반환.
   - 논블로킹: SnapshotCh가 가득 차도 엔진 루프가 막히지 않는다(매칭 계속).
   - depth 클램프/truncate: 요청 depth로 잘린 스냅샷.
   - graceful stop 시 마지막 flush 반영.
2. **핸들러**: orderbook REST가 캐시 기반으로 정확한 스냅샷/depth를 반환.
3. **전체 스위트** 회귀.
4. **GCP 재측정**: 코얼레싱 효과(주문마다 스냅샷 생성 소거)는 현재 벤치마크(WS
   없음)로도 매칭 처리량 향상으로 측정 가능. 같은 세션 A/B(before=현재 HEAD,
   after=이 변경). 결박 해소(WS 부하 시)는 WS 시나리오가 필요 — 후속.
