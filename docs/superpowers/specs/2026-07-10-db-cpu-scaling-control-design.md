# DB 인스턴스 CPU 증설 대조군 실험 설계

## 배경 (왜 필요한가)

`14-2026-07-10-pg-stat-statements-investigation.md`에서 Postgres CPU 포화(389%)의 원인이 느린 쿼리가 아니라 정산 트랜잭션당 약 15회의 쿼리 왕복 구조임을 확인했다. 쿼리 자체는 전부 빠르므로(평균 0.18~1.46ms), 코드를 건드리는 리스크 있는 리팩터링보다 먼저 DB 인스턴스의 CPU를 늘려서 이 순수 용량 문제가 해소되는지 실측한다.

## 왜 이 방식을 선택했는지

`07-2026-07-09-cpu-core-pinning.md`, `11-2026-07-09-vm-cpu-scaling-control.md`에서 확립한 것과 같은 원칙 — **먼저 `gcloud`로 일시 리사이즈해서 실측하고, 결과에 따라 Terraform에 정식 반영하거나 되돌린다.** 이번엔 이미 두 차례(서버, 부하생성기) 검증된 절차라 새로운 리스크는 적다.

## 범위

- DB 인스턴스(`goexchange-stress-db`)를 `e2-medium`(2 vCPU) → `e2-highcpu-4`(4 vCPU)로 리사이즈한다.
- 서버/부하생성기 인스턴스는 건드리지 않는다.
- 실제 리사이즈, k6 재실행, 결과 문서화, Terraform 갱신 여부 결정은 사용자와 직접 진행한다.

## 아키텍처

### 리사이즈 절차 (11번 테스트와 동일 패턴)

```bash
gcloud compute instances stop goexchange-stress-db --zone=asia-northeast3-a
gcloud compute instances set-machine-type goexchange-stress-db --zone=asia-northeast3-a --machine-type=e2-highcpu-4
gcloud compute instances start goexchange-stress-db --zone=asia-northeast3-a
```

리사이즈 후 외부/내부 IP를 재확인한다(외부 IP는 바뀔 수 있음, 내부 IP `10.10.0.4`는 유지될 가능성이 높지만 확인 필요).

### 검증 절차 (사용자와 직접 진행, 이 스펙의 범위 밖)

1. 리사이즈 후 `nproc`/`free -h`로 사양 확인.
2. `pg_stat_statements_reset()`로 통계 초기화.
3. k6를 14번과 동일 조건(VU~1000대까지)으로 재실행.
4. Postgres CPU(`docker stats`, `uptime`), `pg_stat_statements` 누적 실행시간, `execution` 채널 길이, 전체 p95/처리량을 13/14번 문서와 비교.
5. 결과를 `docs/benchmarks/15-YYYY-MM-DD-db-cpu-scaling-control.md`에 기록.

### 판단 기준

- **유지**: Postgres CPU 포화가 해소되고 전체 처리량/지연이 뚜렷하게 개선되면 → `infra/terraform/gcp/variables.tf`의 `db_machine_type` 기본값을 `e2-highcpu-4`로 갱신.
- **되돌림**: 개선이 없거나 오히려 나빠지면 → `gcloud`로 `e2-medium`으로 되돌리고 Terraform은 그대로 둔다.

## 성공 기준

- DB CPU 증설이 13/14번에서 확인한 "쿼리 자체는 빠른데 호출량이 많다"는 병목을 실제로 해소하는지 명확한 실측 근거를 얻는다.

## 범위 밖 (Out of Scope)

- 정산 트랜잭션 쿼리 왕복 횟수를 줄이는 코드 리팩터링 — 이번 실험 결과를 보고 필요성을 재평가.
- Redis/Kafka 도입 — 사용자와 논의한 대로, 지금 확인한 병목에 직접 대응하지 않는 것으로 판단해 이번 스코프에서 제외.
