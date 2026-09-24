# GCP 분리 환경 스트레스 테스트 실행 런북

`docs/superpowers/specs/2026-07-06-gcp-stress-test-design.md`의 설계를 실제로 실행하는 순서입니다.
이 문서의 단계들은 실제 GCP 비용이 발생하므로, 각 단계를 실행하기 전에 내용을 이해하고 진행하세요.

## 1. 인프라 생성

`infra/terraform/gcp/README.md`를 따라 `terraform apply`를 실행합니다. 완료되면 아래 출력값을 기록해둡니다.

```bash
terraform output
```

- `server_external_ip`, `server_internal_ip`
- `load_gen_external_ip`
- `server_ssh_command`, `load_gen_ssh_command`

## 2. 서버 인스턴스에 코드 배포

로컬에서 저장소를 서버 인스턴스로 복사합니다 (git clone도 가능하지만, 사설 저장소라면 scp가 더 간단합니다).

```bash
scp -r -i ~/.ssh/goexchange-gcp . goexchange@<server_external_ip>:~/go-exchange-back
```

## 3. 환경변수 파일 준비

서버 인스턴스에 SSH 접속 후:

```bash
ssh -i ~/.ssh/goexchange-gcp goexchange@<server_external_ip>
cd ~/go-exchange-back
cp .env.stress.example .env
# .env를 열어 POSTGRES_PASSWORD, GOEXCHANGE_JWT_SECRET, GOEXCHANGE_DEV_TOOLS_TOKEN,
# GRAFANA_ADMIN_PASSWORD를 실제 값으로 채운다.
```

## 4. 스택 기동

```bash
docker compose -f docker-compose.stress.yml up -d --build
docker compose -f docker-compose.stress.yml ps
```

`backend`, `postgres`, `prometheus`, `grafana`, `node-exporter`, `postgres-exporter` 6개 컨테이너가 모두 `Up` (backend/postgres는 `healthy`)이어야 한다.

## 5. Grafana 확인

로컬 브라우저에서 `http://<server_external_ip>:3000`에 접속해, `admin` / `.env`에 설정한 `GRAFANA_ADMIN_PASSWORD`로 로그인한다. "GoExchange Stress Test" 대시보드가 보이는지 확인한다.

## 6. 부하생성 인스턴스에서 k6 실행

```bash
ssh -i ~/.ssh/goexchange-gcp goexchange@<load_gen_external_ip>
sudo snap install k6   # 또는 https://k6.io/docs/get-started/installation/ 의 우분투 설치 방법
```

로컬에서 `loadtest/order-submission-stress.js`를 부하생성 인스턴스로 복사한 뒤 실행한다.

```bash
scp -i ~/.ssh/goexchange-gcp loadtest/order-submission-stress.js goexchange@<load_gen_external_ip>:~/order-submission-stress.js
ssh -i ~/.ssh/goexchange-gcp goexchange@<load_gen_external_ip> \
  "k6 run -e BASE_URL=http://<server_internal_ip>:8080 -e DEV_TOOLS_TOKEN=<GOEXCHANGE_DEV_TOOLS_TOKEN 값> ~/order-submission-stress.js"
```

`server_internal_ip`를 쓰는 이유는 같은 VPC 안에서는 내부 IP가 더 빠르고, 외부 IP 대역폭/과금을 피할 수 있기 때문이다.

### 6.1. setup 단계 인증 rate limit 계약 (설계 §6.4)

`setup()`은 25000명을 100명씩(`SETUP_BATCH_SIZE`) 배치로 가입시킨다. 배치 사이엔
`SETUP_BATCH_INTERVAL_SECONDS = 0.5`초 고정 sleep이 있다(하니스 소스에 직접
박혀 있다 — env로 열지 않는다, 값을 바꾸면 아래 한도도 다시 계산해야 한다).

- `docker-compose.stress.yml`은 이 계약에서 역산한 `GOEXCHANGE_AUTH_RATE_LIMIT_RPS=400`,
  `GOEXCHANGE_AUTH_RATE_LIMIT_BURST=200`을 기본으로 이미 갖고 있다(여유 2배,
  계산 과정은 계획서 Task 9 Step 1 참고). 주문·이체 한도는 사용자당 2~5rps라
  기본값과 충돌하지 않으므로 건드리지 않는다.
- **preflight**: register·login 응답이 429면 `setup()`이 `setup preflight: ...
  rate limited (429)`로 즉시 실패한다(k6 실행 자체가 에러로 종료된다). k6 실행이
  `setup preflight` 에러 없이 `submitOrders` 단계로 넘어갔다면 preflight를
  통과한 것이다 — 별도 커맨드가 필요 없다.
- 이 에러가 나면 본 실행을 진행하지 말고 `GOEXCHANGE_AUTH_RATE_LIMIT_RPS`·
  `BURST`를 올리거나 `SETUP_BATCH_INTERVAL_SECONDS`를 늘린 뒤(둘 다 바꾸면
  위 계산을 다시 한다) 재실행한다.
- 실행 결과(이 실행에서 429가 있었는지, 있었다면 어떤 조치로 해소했는지)는
  8번 단계의 `docs/benchmarks/03-YYYY-MM-DD-gcp-stress-test.md`에 함께 남긴다.

**운영 배포에서의 인증 한도(공유 IP).** 인증 limiter의 키는 클라이언트 IP다. CGNAT·
사내 NAT·학교 망처럼 같은 공인 IP를 여러 정상 사용자가 공유하면, 그 IP 하나가 기본
`1rps/burst 10` 버킷을 나눠 쓰다 정상 사용자가 429를 맞을 수 있다. 배포별로 이런
환경이 예상되면 `GOEXCHANGE_AUTH_RATE_LIMIT_RPS`·`BURST`를 올린다(기준: 그 IP 뒤의
예상 동시 가입·로그인 사용자 수를 burst로, 초당 평균 시도 수를 rps로 잡는다). 관측은
관리 포트(`:9101/metrics`)의 `http_requests_total{path="/auth/login"|"/auth/register", status="429"}`
증가율(`rate(...[1m])`)과 응답의 `Retry-After` 헤더·`RATE_LIMITED` 코드로 한다. 로드밸런서·
프록시 뒤라면 `GOEXCHANGE_TRUSTED_PROXIES`에 그 CIDR을 넣어야 클라이언트 IP가 프록시 IP
하나로 뭉치지 않는다. 이 값들은 **compose가 `backend.environment`로 명시 전달해야만**
컨테이너에 도달한다(`.env`에만 적으면 반영되지 않는다) — `docker-compose.*.yml`에 해당
키가 이미 나열돼 있으니 `.env`의 값을 바꾸고 `docker compose up -d`로 재기동한다.

## 6.5. (선택) CPU 프로파일 캡처

이전 실행에서 CPU 포화가 관측된 VU 구간(예: 150~200)이 있다면, 그 구간에서 30초 CPU 프로파일을 캡처해 실제 병목 함수를 확인할 수 있다.

1. `.env`에 `GOEXCHANGE_ENABLE_PPROF=true`가 설정된 채로 서버가 기동 중인지 확인한다 (기본값은 `false`이므로 명시적으로 켜야 한다).
2. 로컬에서 서버 인스턴스로 SSH 터널을 연다(관리 서버 — 설계 §7. `/metrics`도
   같은 포트다):
   ```bash
   ssh -L 9101:localhost:9101 -i ~/.ssh/goexchange-gcp goexchange@<server_external_ip>
   ```
3. k6가 목표 VU 구간에 진입한 시점에, 로컬의 또 다른 터미널에서 프로파일을 받는다:
   ```bash
   go tool pprof -seconds=30 -output=cpu.prof http://localhost:9101/debug/pprof/profile
   ```
4. 캡처가 끝나면 분석한다:
   ```bash
   go tool pprof -top cpu.prof
   go tool pprof -svg cpu.prof > cpu.svg
   ```
5. 결과를 `docs/benchmarks/04-YYYY-MM-DD-matching-engine-cpu-profiling.md`에 기록한다 (왜 이 조사를 했는지, `-top` 원본 출력, 상위 CPU 소비 함수 요약, 다음 작업 제안 포함).

## 7. 실시간 관찰과 수동 종료

Grafana 대시보드(5번 단계에서 연 탭)를 계속 보면서, 다음 중 하나가 관측되면 k6 실행 터미널에서 `Ctrl+C`로 중단한다.

- `http_req_failed`(또는 `create_order` 태그의 에러율)이 급격히 증가
- HTTP p95 응답시간이 이전 단계 대비 몇 배 이상 급격히 저하
- CPU/메모리 패널이 포화(90%+)에 근접하고 다른 지표도 함께 무너짐
- Postgres 커넥션 수가 한계에 도달하는 신호

어느 패널이 가장 먼저 무너지는지, 몇 VU 근처에서 그랬는지를 기록해둔다.

## 7.5. 산출물 시크릿 게이트 (필수)

**동기화 대상 경로(`_workspace/`, `_artifacts/`, OneDrive 아래 어디든)에는 시크릿 스캔을
통과한 정리본만 진입한다. 원본 summary는 진입하지 않는다.**

k6의 `--summary-export`는 `setup()`의 반환값을 `setup_data`에 통째로 덤프한다. 이 저장소의
부하 하니스는 사용자별 JWT를 거기에 담으므로, 정리하지 않은 summary는 토큰 수백~수천 개를
그대로 들고 있다. 34번·35번에서 실제로 그렇게 남아 사후 정리와 클라우드 버전 기록 확인이
필요했다. 순서를 지키면 그 일이 생기지 않는다.

| # | 단계 | 실패 시 |
|---|---|---|
| 1 | summary 원본은 **원격 VM 또는 OneDrive 밖 임시 경로**에만 생성한다 | — |
| 2 | `setup_data`를 redaction metadata로 치환한다 | — |
| 3 | metrics가 정리 전후 **동일**한지 검증한다 | **중단** |
| 4 | JWT·`"token"`·Bearer·Authorization·`GOEXCHANGE_JWT_SECRET` 패턴을 스캔한다 | — |
| 5 | 히트가 하나라도 있으면 **packaging·복사를 중단**한다 | **중단** |
| 6 | 통과한 파일만 tgz로 만들고 checksum을 계산한다 | — |
| 7 | **그 이후에만** `_workspace/`·`_artifacts/`로 복사한다 | — |

2~5는 스크립트가 수행하고, 실패하면 **exit 2**로 멈춘다.

```bash
# VM 또는 OneDrive 밖 임시 경로에서 (2~5단계)
python _workspace/loadtest/redact_summary.py <phase>-summary-a.json <phase>-summary-b.json

# 6단계: 통과한 뒤에만 packaging
tar czf <phase>-loadgen-a.tgz <phase>-summary-a.json <phase>-stdout-a.log <phase>/
sha256sum *.tgz > checksums.txt

# 7단계 직전 최종 확인 — 디렉터리 재귀 스캔
python _workspace/loadtest/redact_summary.py --scan-only <staging-dir>
```

tgz를 만든 뒤에 정리하면 압축 내부가 남으므로 **반드시 packaging 전에** 수행한다.
이미 만들어진 tgz를 정리하려면 풀어서 정리하고 **멤버 이름·순서를 보존해 재생성**한 뒤
checksum을 갱신하고, 정리 전/후 checksum을 `redaction-manifest.json`에 남긴다.

스크립트 자체의 회귀는 `python _workspace/loadtest/redact_summary_test.py`로 고정돼 있다
(JWT fixture 입력에서 `setup_data` 제거 · metrics 불변 · 스캔 0건 · 히트 시 exit 2).

> 이 게이트는 **측정 산출물**에 적용한다. 하니스 소스 자체를 스캔하면 스크립트의 패턴
> 리터럴과 테스트 fixture가 히트로 잡히는데, 둘 다 실제 시크릿이 아니다.

## 8. 결과 기록

k6 종료 시 출력되는 요약과, Grafana 대시보드 스크린샷(문제가 시작된 시점 전후)을 캡처해서 기존 컨벤션대로 저장한다.

- `docs/benchmarks/03-YYYY-MM-DD-gcp-stress-test.md` 생성 (형식은 `docs/benchmarks/README.md` 참고)
- k6 요약, 병목이 관측된 VU 구간, 병목 원인(CPU/메모리/GC/DB 커넥션/매칭엔진 큐잉 중 무엇이었는지) 서술
- Grafana 스크린샷은 `docs/benchmarks/`에 이미지 파일로 함께 커밋하거나, 스크린샷 없이 관측한 수치(예: "CPU 92%, p95 1.2s, order_pipeline_match_latency_seconds p95 3.4s")를 텍스트로 남긴다
- `docs/benchmarks/README.md` 목록에 항목 추가

## 9. 인스턴스 정리

결과 기록 및 추가 분석이 끝나면(며칠 이내), 비용을 막기 위해 인스턴스를 삭제한다.

```bash
cd infra/terraform/gcp
terraform destroy
```
