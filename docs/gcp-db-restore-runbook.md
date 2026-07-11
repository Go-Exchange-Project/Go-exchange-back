# DB 스냅샷 복원 리허설 런북

복원해 본 적 없는 백업은 백업이 아니다. 이 런북은 자동 스냅샷(C-2)에서 DB를
복원해 데이터 정합성까지 확인하는 절차다. **분기 1회 이상, 그리고 첫 스냅샷 생성
직후 1회는 반드시 수행한다.**

- RPO(최대 데이터 손실): 일일 스냅샷 기준 최악 24시간. 이를 줄이려면 WAL-G PITR
  도입(백로그)이 필요하다.
- 비용: 리허설 중 임시 디스크+VM이 생성된다. **완료 후 즉시 삭제**(마지막 단계).

## 0. 사전 조건

```bash
gcloud config set project <PROJECT_ID>
export ZONE=asia-northeast3-a
export DISK=goexchange-stress-db   # DB 부트 디스크 이름(인스턴스 이름과 동일)
```

- IAP SSH 사용을 위해 계정에 `roles/iap.tunnelResourceAccessor`가 필요하다.
- 첫 리허설이라 자동 스냅샷이 아직 없다면 수동으로 하나 만든다:

```bash
gcloud compute disks snapshot $DISK --zone=$ZONE \
  --snapshot-names=manual-rehearsal-$(date +%Y%m%d)
```

## 1. 복원할 스냅샷 선택

```bash
gcloud compute snapshots list --filter="sourceDisk ~ $DISK" \
  --sort-by=~creationTimestamp --format="table(name, creationTimestamp, diskSizeGb)"
export SNAPSHOT=<위 목록에서 선택한 이름>
```

## 2. 스냅샷 → 임시 디스크 → 임시 VM

```bash
gcloud compute disks create rehearsal-db-disk --zone=$ZONE \
  --source-snapshot=$SNAPSHOT --type=pd-ssd

gcloud compute instances create rehearsal-db --zone=$ZONE \
  --machine-type=e2-small \
  --disk=name=rehearsal-db-disk,boot=yes,auto-delete=no \
  --network=goexchange-stress-vpc --subnet=goexchange-stress-subnet \
  --no-address --tags=goexchange-db
```

`--tags=goexchange-db`라서 기존 IAP SSH 방화벽 규칙이 그대로 적용된다.
`--no-address`이므로 운영 DB와 마찬가지로 공인망에 노출되지 않는다.

## 3. 접속 및 Postgres 기동 확인

```bash
gcloud compute ssh rehearsal-db --zone=$ZONE --tunnel-through-iap
```

VM 안에서:

```bash
docker ps          # postgres 컨테이너가 자동 시작 안 됐으면:
cd <compose 디렉토리> && docker compose up -d
docker logs <postgres-container> --tail 20
# "database system was not properly shut down; automatic recovery in progress"
# 후 "database system is ready to accept connections"가 나오면 정상 —
# crash-consistent 스냅샷을 WAL 복구가 처리한 것이다.
```

## 4. 데이터 정합성 검증

행 수 확인:

```sql
SELECT (SELECT count(*) FROM users)   AS users,
       (SELECT count(*) FROM orders)  AS orders,
       (SELECT count(*) FROM trades)  AS trades,
       (SELECT count(*) FROM wallets) AS wallets,
       (SELECT count(*) FROM ledger_entries) AS ledger_entries;
```

리컨실리에이션 검사 1(원장-지갑 일치) — 위반이면 행이 나온다(0행이 정상):

```sql
SELECT w.id, w.user_id, w.coin_symbol,
       w.available_balance - COALESCE(l.a_sum, 0) AS available_gap,
       w.locked_balance    - COALESCE(l.l_sum, 0) AS locked_gap
FROM wallets w
LEFT JOIN (
  SELECT user_id, coin_symbol,
         SUM(available_delta) AS a_sum, SUM(locked_delta) AS l_sum
  FROM ledger_entries GROUP BY user_id, coin_symbol
) l ON l.user_id = w.user_id AND l.coin_symbol = w.coin_symbol
WHERE w.available_balance <> COALESCE(l.a_sum, 0)
   OR w.locked_balance    <> COALESCE(l.l_sum, 0);
```

리컨실리에이션 검사 2(자산별 총량 보존) — `diff`가 전부 0이어야 정상:

```sql
WITH wallet_totals AS (
  SELECT coin_symbol, SUM(available_balance + locked_balance) AS total
  FROM wallets GROUP BY coin_symbol
),
fee_totals AS (
  SELECT COALESCE(SUM(buyer_fee), 0) + COALESCE(SUM(seller_fee), 0) AS total FROM trades
),
funded_totals AS (
  SELECT coin_symbol, SUM(available_delta) AS total
  FROM ledger_entries WHERE entry_type = 'DEV_FUND' GROUP BY coin_symbol
)
SELECT w.coin_symbol,
       w.total
         + CASE WHEN w.coin_symbol = 'KRW' THEN (SELECT total FROM fee_totals) ELSE 0 END
         - COALESCE(f.total, 0) AS diff
FROM wallet_totals w
LEFT JOIN funded_totals f ON f.coin_symbol = w.coin_symbol;
```

(더 완전한 검증을 원하면 goexchange 서버를 이 DB에 붙여 기동 — 기동 직후
리컨실리에이션이 자동 1회 실행되어 `reconciliation_violations` 게이지/로그로
검사 3종 전부를 확인할 수 있다.)

## 5. 정리 (필수 — 비용)

```bash
gcloud compute instances delete rehearsal-db --zone=$ZONE --quiet
gcloud compute disks delete rehearsal-db-disk --zone=$ZONE --quiet
# 수동 스냅샷을 만들었다면:
gcloud compute snapshots delete manual-rehearsal-<날짜> --quiet
```

## 6. 기록

리허설 결과(스냅샷 이름, 복원 소요 시간, 검사 1·2 결과)를
`docs/refactor/`의 C-2 완료 문서에 남긴다.
