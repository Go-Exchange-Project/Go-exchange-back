# Runbook: corrupted/undurable outbox 이벤트 수동 복구

4차 리팩토링 축1(A+C: per-order 런타임 fence + terminal durable defer)에서
`OutboxReplayer`가 fail-closed로 바뀌면서, 부팅 시점 replay가 아래 두 상황
중 하나를 만나면 **자동으로 격리하거나 건너뛰지 않고 즉시 중단**한다:

- `Process()`가 `false`를 반환(비내구적 처리, undurable)
- `event_type`/`payload`가 손상돼 디코딩 자체가 실패(corrupted)

두 경우 모두 서버가 뜨지 않는다. 이는 버그가 아니라 의도된 계약이다 —
정합성이 확인되지 않은 상태로 라이브 트래픽을 받기 시작하면 더 위험하다.

## 1. 증상 확인

부팅 실패 로그에서 다음 패턴을 찾는다:

```
outbox replay: CORRUPTED event <id>: ...
```

또는

```
outbox event <id> not durably handled
```

`<id>`가 문제의 `trade_outbox_events.id`다.

## 2. payload 검토

```sql
SELECT * FROM trade_outbox_events WHERE id = <id>;
```

`event_type`, `payload`, `status`, `created_at`을 확인해 다음을 판단한다:

- `payload`가 단순 오타·타입 오류로 복구 가능한가
- 아니면 원본 이벤트가 근본적으로 재구성 불가능한가(예: 필수 필드 자체가 없음)

## 3. 판단 후 처리 — 사람이 직접 결정한다

- **복구 가능**: `payload`를 수정해 정상 디코딩되도록 만든 뒤 서버를 재기동한다.
  리플레이가 정상적으로 이 행을 처리하고 `PROCESSED`로 마킹한다.
- **복구 불가능**: 사람이 명시적으로 상태를 전환하고 사유를 기록한다.

  ```sql
  UPDATE trade_outbox_events
     SET status = 'PROCESSED'
   WHERE id = <id>;
  ```

  이 UPDATE를 실행한 사람·시각·사유를 반드시 별도로 기록한다(이슈 트래커,
  운영 로그 등 — 이 저장소는 그 기록을 위한 스키마를 두지 않는다).

## 4. 새 엔드포인트·스키마를 만들지 않는다

이 절차는 **의도적으로 수동**이다. 자동 복구 API나 전용 격리 테이블을
추가하지 않는다 — corrupted/undurable 이벤트는 드물어야 하고, 자동화는
"확인 없이 넘어가는" 옛 동작을 다른 이름으로 되살리는 것과 같다.

## crash loop는 정상 동작이다

`Process()`가 `false`를 반환하는 원인이 DB 연결 문제 등 일시적 인프라
이상이라면, DB가 회복될 때까지 서버가 계속 부팅에 실패하며 재시작을
반복한다(`crash loop`). 이것은 버그가 아니라 계약이다 — DB가 회복되기
전까지 서버가 뜨지 않는 것이 안전한 기본값이다. 이 경우 별도 조치 없이
DB 인프라를 복구하면 다음 재시작에서 정상적으로 replay가 이어진다.
