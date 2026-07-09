# VM 코어 수 대조군 실험 설계

## 배경 (왜 필요한가)

`10-2026-07-09-settlement-worker-pool.md`에서 정산 워커 병목을 해소했더니, CPU 사용률이 99.3%까지 치솟으면서도 `ExecutionCh`/`OrderCh` 채널 포화와 매칭 지연 최댓값(26.5초)이 거의 그대로였다. 이건 병목이 "정산 워커 부족"에서 "CPU 자체의 물리적 한계"로 옮겨갔다는 강한 신호다.

이번 작업은 이 가설을 직접 검증한다 — 서버 인스턴스의 코어 수를 2배로 늘려서 같은 조건으로 재측정했을 때, 병목이 실제로 풀리는지 확인한다. [[goexchange-performance-goal]]이 최근 업데이트되어(사용량 대비 넉넉한 GCP 무료 크레딧 확인), VM 사양 조정이 이제 옵션에 들어왔다 — 다만 "일단 올리고 본다"가 아니라, **실측으로 확인한 뒤 결과에 따라 유지할지 되돌릴지 정하는 대조군 실험**으로 진행한다. `07번 테스트`(CPU 코어 핀닝)에서도 같은 원칙(실험 → 측정 → 근거 있으면 유지, 아니면 되돌림)을 적용했다.

## 왜 이 방식을 선택했는지

머신 타입은 `e2-highcpu-4`(4 vCPU, 4GB RAM)로 골랐다 — 코어 수만 정확히 2배로 늘리고 RAM은 그대로 둬서, "CPU가 병목이었는가"라는 질문에 다른 변수(메모리 여유) 없이 순수하게 답할 수 있게 했다.

인프라 변경은 두 단계로 나눈다: (1) 실험 단계에서는 `gcloud compute instances stop/set-machine-type/start`로 서버 인스턴스만 직접 리사이즈한다 — Terraform 상태는 건드리지 않아서, 결과가 안 좋으면 되돌리기가 즉시(리사이즈만 반대로 한 번 더) 가능하다. (2) 결과가 확실히 좋으면, 그때 `infra/terraform/gcp/variables.tf`의 `server_machine_type` 기본값을 갱신해서 IaC가 실제 인프라 상태와 일치하게 만들고 정식 커밋한다.

## 범위

- GCP `gcloud` CLI로 서버 인스턴스(`8.230.15.177`)만 리사이즈한다 — load-gen 인스턴스는 그대로 둔다.
- Terraform 코드 변경은 결과가 확정된 뒤(유지하기로 결정한 경우)에만 한다 — 이번 스펙은 실험 절차와 판단 기준까지만 다룬다.
- k6 재실행/결과 문서화는 이 스펙 문서화 이후 사용자와 직접 진행한다.

## 아키텍처

### 1. 서버 인스턴스 리사이즈 (실험)

```bash
gcloud compute instances stop <server-instance-name> --zone=<zone>
gcloud compute instances set-machine-type <server-instance-name> --zone=<zone> --machine-type=e2-highcpu-4
gcloud compute instances start <server-instance-name> --zone=<zone>
```

리사이즈 후 인스턴스의 외부 IP가 바뀔 수 있으므로(고정 IP가 아니라면), 재시작 후 `gcloud compute instances describe`로 IP를 다시 확인하고 이후 배포/테스트 커맨드에 반영한다.

### 2. 재배포 및 검증 절차 (사용자와 직접 진행, 이 스펙의 범위 밖)

1. 리사이즈된 인스턴스에 최신 코드(`d50e383` 이후, 정산 워커 풀 포함) 재배포.
2. k6 스트레스 테스트를 04~10번과 동일한 조건으로 재실행.
3. 다음 지표를 09/10번 문서와 비교:
   - `matching_engine_channel_length{channel="execution"|"order"}` — 여전히 1024까지 포화되는지, 아니면 여유가 생기는지
   - `order_pipeline_match_latency_seconds` p95/최댓값
   - CPU 사용률(전체 코어 평균)
   - k6 최종 처리량/p95, 같은 VU(150) 시점 누적 iteration
4. 결과를 `docs/benchmarks/11-YYYY-MM-DD-vm-cpu-scaling-control.md`에 기록 — 09/10번을 기준선으로 전/후 비교.

### 3. 판단 기준 (유지 vs 되돌림)

- **유지**: 채널 포화가 해소되거나 크게 완화되고, 매칭 지연 최댓값이 뚜렷하게 줄어든 경우 → `infra/terraform/gcp/variables.tf`의 `server_machine_type` 기본값을 `e2-highcpu-4`로 갱신해서 정식 커밋한다.
- **되돌림**: 07번 테스트(CPU 핀닝)처럼 별 차이가 없거나 오히려 나빠진 경우 → `gcloud`로 다시 `e2-medium`으로 리사이즈하고, Terraform은 애초에 안 건드렸으므로 추가 정리가 필요 없다.
- 결과가 애매하면(일부만 개선) 있는 그대로 기록하고, 판단 근거를 명시한 뒤 사용자와 상의해서 결정한다.

## 성공 기준

- 코어를 2배로 늘렸을 때 병목이 풀리는지 아닌지에 대한 명확한 실측 근거를 얻는다(개선이든 아니든 과장 없이 기록).
- 최종적으로 서버가 `e2-medium`과 `e2-highcpu-4` 중 하나로 확정되고, 그 상태가 Terraform 코드(유지하는 경우) 또는 실제 인스턴스 상태(되돌리는 경우) 어느 쪽이든 일관되게 반영된다.

## 범위 밖 (Out of Scope)

- load-gen 인스턴스 사양 변경.
- 이번 실험 결과에 따른 추가 아키텍처 변경(Redis/Kafka 등) — 로드맵의 다음 단계.
