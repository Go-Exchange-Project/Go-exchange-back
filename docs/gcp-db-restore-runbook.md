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

> 복식 부기 전환(migration 009·010) 이후 `wallets`·`ledger_entries`는 존재하지 않는다.
> 아래 검사는 `ReconciliationWorker`의 검사 1~4와 같은 질의다.

행 수 확인:

```sql
SELECT (SELECT count(*) FROM users)            AS users,
       (SELECT count(*) FROM orders)           AS orders,
       (SELECT count(*) FROM trades)           AS trades,
       (SELECT count(*) FROM accounts)         AS accounts,
       (SELECT count(*) FROM journal_entries)  AS journal_entries,
       (SELECT count(*) FROM postings)         AS postings;
```

검사 1(분개 불균형) — 한 분개 안에서 자산별 전기 합은 0이어야 한다(0행이 정상):

```sql
SELECT journal_id, asset, SUM(amount) AS sum
FROM postings
GROUP BY journal_id, asset
HAVING SUM(amount) <> 0
ORDER BY journal_id, asset;
```

검사 2(잔고 캐시 drift) — `account_balances`가 전기 합과 같아야 한다(0행이 정상):

```sql
SELECT a.id AS account_id,
       COALESCE(b.balance, 0)  AS cached,
       COALESCE(p.computed, 0) AS computed
FROM accounts a
LEFT JOIN account_balances b ON b.account_id = a.id
LEFT JOIN LATERAL (
  SELECT SUM(amount) AS computed FROM postings WHERE account_id = a.id
) p ON true
WHERE COALESCE(b.balance, 0) IS DISTINCT FROM COALESCE(p.computed, 0)
ORDER BY a.id;
```

검사 3(자산별 총량 보존) — 외부·MINT 계정이 반대편을 받으므로 자산별 전체 합은 정확히 0이다(0행이 정상):

```sql
SELECT asset, SUM(amount) AS sum
FROM postings
GROUP BY asset
HAVING SUM(amount) <> 0
ORDER BY asset;
```

검사 4(음수 계정) — 음수를 허용하지 않는 계정이 음수면 없는 돈을 쓴 것이다(0행이 정상):

```sql
SELECT a.id AS account_id, a.account_type, a.owner_user_id, a.asset, b.balance
FROM accounts a
JOIN account_balances b ON b.account_id = a.id
WHERE a.allows_negative = false
  AND b.balance < 0
ORDER BY a.id;
```

(더 완전한 검증을 원하면 goexchange 서버를 이 DB에 붙여 기동 — 기동 직후
리컨실리에이션이 자동 1회 실행되어 `reconciliation_violations` 게이지/로그로
검사 전부를 확인할 수 있다. 서버 기동에는 `GOEXCHANGE_JWT_SECRET`이 필요하다.)

## 5. 정리 (필수 — 비용)

```bash
gcloud compute instances delete rehearsal-db --zone=$ZONE --quiet
gcloud compute disks delete rehearsal-db-disk --zone=$ZONE --quiet
# 수동 스냅샷을 만들었다면:
gcloud compute snapshots delete manual-rehearsal-<날짜> --quiet
```

## 6. 기록

리허설 결과(스냅샷 이름, 복원 소요 시간, 검사 1~4 결과)를
`docs/refactor/`의 C-2 완료 문서에 남긴다.
