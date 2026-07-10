# 11번째 테스트 (2026-07-09): VM 코어 수 대조군 실험 (2 vCPU → 4 vCPU)

## 커밋 해시

`893d548` (docs: VM CPU 스케일링 대조군 실험 구현 계획 추가) — 이 테스트 자체는 코드 변경 없이 인프라(GCP 인스턴스 리사이즈)만 바꿔서 진행했다. 애플리케이션 코드는 `d50e383`(정산 워커 풀) 이후 변경 없음.

## 왜 이 테스트를 했는지

`10-2026-07-09-settlement-worker-pool.md`에서 정산 워커 병목을 없앴는데도 CPU가 99.3%까지 치솟고 채널 포화·매칭 지연 최댓값(26.5초)이 거의 그대로였다. 병목이 "정산 워커 부족"에서 "CPU 물리적 한계"로 옮겨갔다는 가설을 세우고, 코어를 2배로 늘려서 이 가설을 실측으로 검증했다.

## 왜 이 방식을 선택했는지

`docs/superpowers/specs/2026-07-09-vm-cpu-scaling-control-design.md`에서 결정한 대로, `e2-medium`(2 vCPU/4GB) → `e2-highcpu-4`(4 vCPU/4GB)로 리사이즈했다 — RAM은 그대로 두고 코어 수만 정확히 2배로 늘려서, "CPU가 병목이었는가"에 다른 변수 없이 답할 수 있게 했다. `gcloud`로 서버 인스턴스만 직접 리사이즈하고(Terraform 상태는 안 건드림), 결과를 보고 유지할지 되돌릴지 정하는 대조군 실험으로 진행했다.

## 환경

- 서버 인스턴스: GCP `e2-highcpu-4`(4 vCPU, 4GB) — 기존 `e2-medium`(2 vCPU)에서 리사이즈
- 외부 IP가 `8.230.15.177` → `34.50.18.121`로 바뀜(고정 IP 아님) — 내부 IP(`10.10.0.3`)는 그대로라 k6 스크립트는 수정 없이 재사용
- DB 커넥션 풀 튜닝(05번), 정산 워커 풀(10번) 등 지금까지의 코드 개선은 전부 유지된 상태

## 실행한 정확한 커맨드

```bash
k6 run -e BASE_URL=http://10.10.0.3:8080 -e DEV_TOOLS_TOKEN=<토큰> loadtest/order-submission-stress.js
```

## 핵심 결과: 압도적으로 개선됐다

### 1. 최종 k6 요약 비교 — 09/10번 대비

| 지표 | 09번 (2vCPU, 워커1개) | 10번 (2vCPU, 워커10개) | 11번 (**4vCPU**, 워커10개) | 변화(10→11) |
|---|---|---|---|---|
| `http_req_duration` p95 | 7.56s | 6.02s | **1.32s** | **6.02s → 1.32s (약 78% 개선)** |
| `http_req_duration` max | - | 7.41s | **1.73s** | **대폭 개선** |
| 총 완료 iteration | 61,535 | 67,185 | **184,943** | **67,185 → 184,943 (약 2.75배)** |
| 처리량 | 90.59 iter/s | 94.29 iter/s | **274.40 iter/s** | **94.29 → 274.40 (약 2.9배)** |
| `http_req_failed` | - | 1.14% | **0.42%** | **개선** |

### 2. 채널 포화 여부 — 대부분 구간에서 완전히 해소됨

| | 09/10번 (2vCPU) | 11번 (4vCPU) |
|---|---|---|
| VU 150~600 구간 | `execution`/`order` 둘 다 1024로 계속 포화 | **둘 다 0 — 전혀 포화 안 됨** |
| VU 700~800(최고 부하) 구간 | (이 구간까지 도달 시 계속 포화) | `order`만 잠깐 1024로 포화, `execution`은 끝까지 0 |
| 매칭 지연 p95 (VU 150선 부근) | 9.67~14.75초 | **0.029초 (사실상 즉시)** |
| 매칭 지연 p95 최댓값(전체 구간) | 26.5~27.5초 | **4.875초 (VU 700+ 극한 부하에서만)** |

### 3. CPU 사용률

| VU 구간 | CPU (2vCPU, 10번) | CPU (4vCPU, 11번) |
|---|---|---|
| VU~123~161 | 99.3% | 85.2% |
| VU 700+ (최고 부하) | (도달 전 이미 포화) | 93~95.6% |

## 원본 출력 (최종 k6 요약, 11번)

```
  █ TOTAL RESULTS 

    checks_total.......: 184943  274.404071/s
    checks_succeeded...: 100.00% 184943 out of 184943
    checks_failed......: 0.00%   0 out of 184943

    HTTP
    http_req_duration..............: avg=392.4ms  min=1.01ms  med=201.41ms max=1.73s p(90)=1.15s p(95)=1.32s
    http_req_failed................: 0.42%  800 out of 187343
    http_reqs......................: 187343 277.965005/s

    EXECUTION
    iterations.....................: 184943 274.404071/s
    vus............................: 227    min=0             max=797
```

### 램프업 타임라인 (30초 간격 샘플)

```
running (01m12.5s), 000/800 VUs, 0 complete and 0 interrupted iterations
running (01m42.5s), 012/800 VUs, 491 complete and 0 interrupted iterations
running (02m12.5s), 025/800 VUs, 2042 complete and 0 interrupted iterations
running (02m42.5s), 037/800 VUs, 4630 complete and 0 interrupted iterations
running (03m12.5s), 050/800 VUs, 8229 complete and 0 interrupted iterations
running (03m42.5s), 062/800 VUs, 12948 complete and 0 interrupted iterations
running (04m12.5s), 075/800 VUs, 18697 complete and 0 interrupted iterations
running (04m42.5s), 087/800 VUs, 25515 complete and 0 interrupted iterations
running (05m12.5s), 100/800 VUs, 33406 complete and 0 interrupted iterations
running (05m42.5s), 125/800 VUs, 42822 complete and 0 interrupted iterations
running (06m12.5s), 150/800 VUs, 54242 complete and 0 interrupted iterations
running (06m42.5s), 175/800 VUs, 67683 complete and 0 interrupted iterations
running (07m12.5s), 200/800 VUs, 81636 complete and 0 interrupted iterations
running (07m42.5s), 250/800 VUs, 93248 complete and 0 interrupted iterations
running (08m12.5s), 300/800 VUs, 106000 complete and 0 interrupted iterations
running (08m42.5s), 350/800 VUs, 119181 complete and 0 interrupted iterations
running (09m12.5s), 400/800 VUs, 132169 complete and 0 interrupted iterations
running (09m42.5s), 500/800 VUs, 145498 complete and 0 interrupted iterations
running (10m12.5s), 600/800 VUs, 158080 complete and 0 interrupted iterations
running (10m42.5s), 700/800 VUs, 171324 complete and 0 interrupted iterations
running (11m12.5s), 752/800 VUs, 184191 complete and 0 interrupted iterations
```

같은 VU 150 시점 누적 iteration을 09/10번과 비교하면: 35,871(09번) / 34,963(10번) → **54,242(11번, 약 55% 증가)**.

## 해석

1. **가설이 정확히 맞았다 — CPU가 진짜 병목이었다.** 코어를 2배로 늘렸더니, 채널 포화가 VU 600대까지 거의 완전히 사라졌고, 매칭 지연 p95는 VU~150 시점 기준 14.75초→0.029초로 사실상 즉시 처리 수준이 됐다. 처리량은 2.9배, 전체 p95는 78% 개선됐다.
2. **완벽하진 않다 — 최고 부하(VU 700+)에서는 다시 한계가 보인다.** CPU가 93~95%까지 오르고 `order` 채널이 잠깐 포화되며 매칭 지연이 다시 4.875초까지 올라간다. 즉 4 vCPU도 무한한 해결책은 아니고, 부하 규모에 비례해서 여전히 한계가 있다 — 다만 그 한계선이 이전보다 훨씬 뒤로(VU 150대 → VU 700대) 밀려났다.
3. **02~07번에서 시도했던 다른 방법들과 비교하면 이번이 가장 효과적이었다.** DB 풀 튜닝(05번)과 정산 워커 풀(10번)은 각각의 병목을 해소했지만 그 아래 있던 다음 병목을 드러내는 정도였고, CPU 코어 핀닝(07번)은 오히려 역효과였다. **순수하게 컴퓨팅 자원을 늘리는 것이, 지금까지 시도한 코드 레벨 최적화보다 훨씬 큰 폭의 개선을 만들었다.**
4. **판단: 유지한다.** 개선 폭이 크고 명확해서, `e2-highcpu-4`를 유지하기로 결정 — Terraform 설정을 갱신해서 IaC에 반영한다.

## 다음 작업 제안

1. **Terraform 설정 갱신**: `infra/terraform/gcp/variables.tf`의 `server_machine_type` 기본값을 `e2-highcpu-4`로 바꿔서 IaC가 실제 인프라 상태와 일치하게 만든다.
2. VU 700+ 구간의 잔여 병목(다시 CPU 93~95%까지 오르는 현상)을 추가로 조사할지는 우선순위를 다시 논의한다 — 지금까지의 개선만으로도 이력서 스토리로는 충분히 강력하다.

## 범위 밖 (Out of Scope)

- VU 700+ 구간의 추가 최적화 — 별도 브레인스토밍.
- load-gen 인스턴스 사양 변경.
