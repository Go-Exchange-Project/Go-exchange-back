# B-1b: WS 심볼 구독 모델

- 작성: 2026-07-14 (Fable5 설계·구현)
- 로드맵: [docs/refactor/README.md](../../refactor/README.md) 5번 — B-1a(스냅샷
  코얼레싱+캐시) 후속, B-1 완결편

## 왜 필요한가

hub가 모든 메시지를 모든 클라이언트에 뿌린다. 오더북 스냅샷(depth 30 × 심볼 수)이
가장 크고 빈번한데, BTC만 보는 클라이언트도 전 심볼의 스냅샷·체결을 받는다.
클라이언트 N × 심볼 M이면 전송량이 N×M으로 자란다 — 급등락+동시접속 급증 시
writePump 적체 → slow client 강제 해제가 연쇄되는 원인. 관심 심볼만 보내면
전송량이 실제 수요에 비례한다.

## 설계

### 1. 토픽 = 코인 심볼

```go
type Message struct {
    CoinSymbol string // ""면 전역 — 모든 클라이언트에게 (upbit ticker 등)
    Payload    []byte
}
```

`Hub.Broadcast`가 `chan []byte` → `chan Message`로 바뀐다. 발행자가 태깅한다:
- 오더북 스냅샷 소비자(main): `Message{CoinSymbol: snapshot.CoinSymbol, ...}`
- 체결 브로드캐스트(정산 워커): `Message{CoinSymbol: trade.CoinSymbol, ...}`
- upbit ticker: `Message{CoinSymbol: "", ...}` (전역 — 소량이라 필터 불필요)

### 2. 구독 프로토콜 (클라이언트 → 서버)

```json
{"action":"subscribe","coin_symbols":["BTC","ETH"]}
{"action":"unsubscribe","coin_symbols":["BTC"]}
```

readPump(현재 모든 수신 메시지를 버림)가 이 JSON을 파싱해 hub의 Subscribe 채널로
보낸다. 잘못된 메시지는 무시(연결 유지) — 클라이언트 버그가 연결을 죽일 이유 없음.

### 3. 하위 호환: legacy full-feed

**구독 메시지를 한 번도 보내지 않은 클라이언트는 기존처럼 전부 받는다.**
첫 subscribe부터 필터 모드로 전환된다. 기존 프론트엔드가 무수정으로 동작하고,
새 클라이언트만 opt-in으로 대역폭을 아낀다.

### 4. 동시성 모델 유지

구독 상태(client.subscriptions)는 hub goroutine에서만 읽고 쓴다 — readPump는
Subscribe 채널로 보낼 뿐. 기존 "hub 루프 단일 goroutine, 락 없음" 모델 그대로.
클라이언트 해제 시 구독 상태도 함께 소멸(별도 정리 불필요).

## 스코프 밖

- 프론트엔드의 구독 opt-in 적용(별도 리포).
- orderbook/trade 채널 분리 구독(심볼 단위면 충분, 필요 시 후속).
- 구독 ack 메시지.

## 검증 방법

1. **단위(hub)**: 심볼 태깅 메시지가 구독자에게만 감, 미구독 심볼 차단,
   legacy 클라이언트(무구독)는 전부 수신, 전역 메시지(CoinSymbol="")는 모두 수신,
   unsubscribe 후 차단, slow client 드롭 회귀.
2. **단위(handler)**: 구독 JSON 파싱(정상/잘못된 액션/빈 심볼/비JSON 무시).
3. **엔드투엔드(httptest + gorilla dialer)**: 실제 WS 연결 2개 — 구독 클라이언트는
   자기 심볼만, legacy 클라이언트는 전부 수신.
4. **전체 스위트** 회귀. GCP/WS 부하 측정은 "나중에 일괄" 방침대로 별도.
