# 3. C-2/C-1 DB 백업과 네트워크 격리 — 완료

- **완료일**: 2026-07-12 (apply + 복원 리허설 실측 완료)
- **복원 리허설 런북**: [docs/gcp-db-restore-runbook.md](../gcp-db-restore-runbook.md)

## 왜 문제였나

**C-2**: 원장·지갑·체결 데이터가 DB VM의 pd-ssd **디스크 한 장에만** 존재했다.
백업도 WAL 아카이빙도 없어서 디스크 장애 = 전 고객 잔고 소실. 리컨실리에이션(A-6)이
아무리 잘 돌아도 데이터 자체가 사라지면 의미가 없다 — 정합성 100% 목표에서 가장
값싼 보험이 빠져 있었다.

**C-1**: DB VM에 공인 IP(`access_config`)가 붙어 있었다. 방화벽이 막고 있긴 했지만,
돈 데이터가 있는 서버를 공인망에 노출할 이유가 전혀 없었다.

## 어떻게 해결했나

Terraform으로 (plan 검증: `5 add, 1 change(in-place), 0 destroy`):

- **일일 스냅샷 정책**: `google_compute_resource_policy` — 매일 19:00 UTC(=KST 04:00,
  트래픽 최저), 보존 7일(`db_snapshot_retention_days` 변수), 디스크 삭제 시에도
  스냅샷 유지(`KEEP_AUTO_SNAPSHOTS`). DB 부트 디스크에 attach.
- **외부 IP 제거**: db 인스턴스의 `access_config` 삭제 — **in-place 변경이라 재생성/
  다운타임 없음**(리허설 시점에 컨테이너 "Up 32 hours"로 확인).
- **Cloud Router + NAT**: 외부 IP 없는 DB의 egress(도커 이미지 pull, apt)용.
  VM 1대 기준 월 ~$1 수준. 외부 IP를 가진 server/load_gen은 NAT를 타지 않음.
- **IAP 터널 SSH**: 방화벽에 GCP IAP 고정 대역(35.235.240.0/20)→:22 허용,
  `gcloud compute ssh --tunnel-through-iap`로 접속(출력 `db_ssh_command`).

**"복원해 본 적 없는 백업은 백업이 아니다"** — 스냅샷 생성에서 끝내지 않고
복원 리허설까지 런북으로 만들어 실제 수행했다.

## 결과 (복원 리허설 실측, 2026-07-12)

- 수동 스냅샷 → 새 pd-ssd 디스크 → 임시 VM(e2-small, 외부 IP 없음) 기동까지
  **수 분 내** 완료. crash-consistent 스냅샷을 Postgres가 WAL 자동 복구로 처리:
  `database system was not properly shut down; automatic recovery in progress` →
  `redo done` → `ready to accept connections`.
- 복원된 실데이터 규모: users 25,000 / orders 1,397,624 / trades 697,244 /
  wallets 28,000 / ledger_entries 4,223,600.
- **복원본에서 리컨실리에이션 검사 실행: 검사 1(원장-지갑 일치) 위반 0건,
  검사 2(자산 총량 보존) BTC·KRW diff 모두 0.** 백업이 복원 가능할 뿐 아니라
  복원된 데이터의 정합성까지 증명됨 — A-6의 남은 수동 확인 항목("스트레스 DB
  전역 위반 0건")도 이 리허설로 함께 완료. legacy_mismatch도 0건(예상대로
  스트레스 DB에는 레거시 이중 필드 지갑 없음).
- 임시 VM/디스크/수동 스냅샷 삭제 완료(비용 정리). 운영 DB는 IAP SSH 접속,
  컨테이너 healthy, NAT egress(도커 레지스트리 도달) 모두 정상 확인.

## 남은 한계 (백로그)

- RPO 최악 24시간(일일 스냅샷) — 더 줄이려면 WAL-G PITR 도입 필요.
- 리허설은 분기 1회 이상 반복(런북 참조).
