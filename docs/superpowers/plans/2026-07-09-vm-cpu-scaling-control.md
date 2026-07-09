# VM 코어 수 대조군 실험 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 서버 인스턴스(`goexchange-stress-server`)를 `e2-medium`(2 vCPU)에서 `e2-highcpu-4`(4 vCPU, RAM 동일)로 일시 리사이즈해서, 10번 테스트에서 발견한 "CPU가 진짜 병목인가"라는 가설을 대조군 실험으로 검증한다.

**Architecture:** `gcloud compute instances`로 인스턴스를 중지→머신 타입 변경→재시작한다. Terraform 상태는 이번 실험 단계에서는 건드리지 않는다 — 결과가 확정된 뒤(유지 결정 시)에만 `infra/terraform/gcp/variables.tf`를 갱신해서 정식 커밋한다.

**Tech Stack:** `gcloud` CLI.

## Global Constraints

- 리사이즈 대상은 `goexchange-stress-server`(zone: `asia-northeast3-a`) 인스턴스뿐이다 — `goexchange-stress-load-gen`은 건드리지 않는다.
- 목표 머신 타입: `e2-highcpu-4`.
- 이 계획은 리사이즈 절차까지만 다룬다. 재배포, k6 재실행, 결과 문서화, Terraform 갱신 여부 결정은 계획 완료 후 사용자와 직접 진행한다.

---

### Task 1: 서버 인스턴스를 `e2-highcpu-4`로 리사이즈

**Files:**
- 없음 (GCP 인프라 조작, 저장소 파일 변경 없음).

**Interfaces:**
- 없음 (단일 태스크, 이어지는 태스크 없음).

- [ ] **Step 1: 현재 상태 확인**

```bash
export PATH="/c/Users/dksco/google-cloud-sdk/bin:$PATH"
gcloud compute instances describe goexchange-stress-server --zone=asia-northeast3-a --format="value(machineType.basename(),status)"
```

Expected: `e2-medium` / `RUNNING` 출력.

- [ ] **Step 2: 인스턴스 중지**

```bash
gcloud compute instances stop goexchange-stress-server --zone=asia-northeast3-a
```

Expected: 중지 완료 메시지, `gcloud compute instances describe ... --format="value(status)"`가 `TERMINATED`를 반환할 때까지 대기.

- [ ] **Step 3: 머신 타입 변경**

```bash
gcloud compute instances set-machine-type goexchange-stress-server --zone=asia-northeast3-a --machine-type=e2-highcpu-4
```

Expected: 성공 메시지, 에러 없음.

- [ ] **Step 4: 인스턴스 재시작**

```bash
gcloud compute instances start goexchange-stress-server --zone=asia-northeast3-a
```

Expected: 시작 완료 메시지.

- [ ] **Step 5: 변경 및 새 IP 확인**

```bash
gcloud compute instances describe goexchange-stress-server --zone=asia-northeast3-a --format="value(machineType.basename(),status,networkInterfaces[0].accessConfigs[0].natIP)"
```

Expected: `e2-highcpu-4` / `RUNNING` / (외부 IP, 이전과 같을 수도 다를 수도 있음 — 고정 IP가 아니므로 바뀌었으면 이후 배포/테스트 커맨드에 새 IP를 반영해야 한다).

- [ ] **Step 6: SSH 접속 확인 (기존 방화벽 규칙이 새 IP에도 그대로 적용되는지)**

```bash
ssh -i "$HOME/.ssh/goexchange-gcp" -o StrictHostKeyChecking=accept-new -o ConnectTimeout=8 goexchange@<확인된 IP> "nproc; free -h"
```

Expected: `nproc`이 `4`를 출력(코어 4개 확인), `free -h`가 약 4GB 총 메모리를 보여줌.
