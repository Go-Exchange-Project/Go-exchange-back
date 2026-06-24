# EC2 + Docker Compose 배포 가이드

이 문서는 Go Exchange MVP를 AWS EC2 한 대에 Docker Compose로 배포하기 위한 가이드입니다.

현재 목표는 복잡한 Kubernetes 배포가 아니라, 아래 흐름을 먼저 완성하는 것입니다.

```text
GitHub Actions CI 성공
→ Docker 이미지 GHCR에 push
→ EC2에서 GHCR 이미지 pull
→ Docker Compose로 postgres/backend/frontend 실행
→ health check 확인
```

## 배포 구조

```text
EC2
├─ postgres
│  └─ 내부 Docker network에서만 접근
├─ backend
│  ├─ image: ghcr.io/go-exchange-project/go-exchange-back:latest
│  └─ host port 8080
└─ frontend
   ├─ image: ghcr.io/go-exchange-project/go-exchange-front:latest
   └─ host port 80
```

초기 배포에서는 프론트와 백엔드를 각각 `80`, `8080` 포트로 노출합니다.
나중에 Nginx reverse proxy와 HTTPS를 붙이면 외부 공개 포트는 `80/443`만 남기는 구조로 바꿀 수 있습니다.

## 준비된 파일

백엔드 저장소에 배포용 파일을 둡니다.

```text
docker-compose.prod.yml
.env.prod.example
docs/EC2_DEPLOYMENT.md
```

`docker-compose.prod.yml`은 로컬에서 이미지를 빌드하지 않습니다.
GitHub Actions가 GHCR에 올린 이미지를 EC2에서 pull해서 실행합니다.

## GitHub Actions 이미지

백엔드 이미지:

```text
ghcr.io/go-exchange-project/go-exchange-back:latest
ghcr.io/go-exchange-project/go-exchange-back:<commit-sha>
```

프론트 이미지:

```text
ghcr.io/go-exchange-project/go-exchange-front:latest
ghcr.io/go-exchange-project/go-exchange-front:<commit-sha>
```

GHCR Package가 private이면 EC2에서 `docker login ghcr.io`가 필요합니다.
처음 배포 학습 단계에서는 GitHub Packages 화면에서 이미지를 public으로 바꾸는 것이 가장 단순합니다.

private package를 유지하려면 GitHub Personal Access Token에 `read:packages` 권한을 부여한 뒤 EC2에서 로그인합니다.

```bash
echo "<GHCR_READ_TOKEN>" | docker login ghcr.io -u "<GITHUB_USERNAME>" --password-stdin
```

## 프론트 이미지 빌드 주소 설정

Vite 앱은 API 주소와 WebSocket 주소가 빌드 시점에 들어갑니다.

EC2 배포 전에 프론트 저장소의 GitHub Repository Variables를 설정해야 합니다.

```text
VITE_API_BASE_URL=http://<EC2_PUBLIC_IP>:8080
VITE_WS_URL=ws://<EC2_PUBLIC_IP>:8080/ws
VITE_ENABLE_DEV_TOOLS=false
```

도메인과 HTTPS를 붙인 뒤에는 아래처럼 바꿉니다.

```text
VITE_API_BASE_URL=https://api.example.com
VITE_WS_URL=wss://api.example.com/ws
VITE_ENABLE_DEV_TOOLS=false
```

이 값을 바꾼 뒤 프론트 저장소에 새 commit을 push해야 새 주소가 반영된 이미지가 GHCR에 올라갑니다.

## EC2 생성 시 권장 설정

처음 학습/포트폴리오 단계에서는 아래 정도면 충분합니다.

```text
AMI: Ubuntu Server 24.04 LTS 또는 22.04 LTS
Instance type: t3.micro 또는 t2.micro
Storage: 20GB 이상
Key pair: 새로 생성 후 pem 파일 안전하게 보관
```

보안 그룹 inbound rule:

```text
22/tcp   내 IP만 허용
80/tcp   0.0.0.0/0
8080/tcp 0.0.0.0/0
```

주의:

```text
5432/tcp PostgreSQL 포트는 외부에 열지 않습니다.
```

8080 포트 공개는 초기 배포용입니다.
운영형 구조에서는 Nginx/ALB/API Gateway 뒤로 숨기는 것이 좋습니다.

## EC2에 Docker 설치

EC2에 SSH 접속한 뒤 실행합니다.

```bash
sudo apt update
sudo apt install -y ca-certificates curl

sudo install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/ubuntu/gpg \
  | sudo gpg --dearmor -o /etc/apt/keyrings/docker.gpg
sudo chmod a+r /etc/apt/keyrings/docker.gpg

source /etc/os-release
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu ${VERSION_CODENAME} stable" \
  | sudo tee /etc/apt/sources.list.d/docker.list > /dev/null

sudo apt update
sudo apt install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin

sudo usermod -aG docker "$USER"
```

`usermod` 이후에는 SSH를 한 번 끊고 다시 접속합니다.

설치 확인:

```bash
docker version
docker compose version
```

## EC2 배포 디렉터리 준비

```bash
sudo mkdir -p /opt/goexchange
sudo chown "$USER":"$USER" /opt/goexchange
cd /opt/goexchange
```

백엔드 저장소의 아래 파일을 EC2의 `/opt/goexchange`에 복사합니다.

```text
docker-compose.prod.yml
.env.prod.example
```

예시는 `scp`를 사용할 수 있습니다.

```bash
scp -i <KEY_PAIR>.pem docker-compose.prod.yml ubuntu@<EC2_PUBLIC_IP>:/opt/goexchange/
scp -i <KEY_PAIR>.pem .env.prod.example ubuntu@<EC2_PUBLIC_IP>:/opt/goexchange/.env.prod
```

EC2에서 `.env.prod`를 수정합니다.

```bash
cd /opt/goexchange
nano .env.prod
```

반드시 바꿀 값:

```text
POSTGRES_PASSWORD
GOEXCHANGE_JWT_SECRET
GOEXCHANGE_CORS_ALLOWED_ORIGINS
GOEXCHANGE_WS_ALLOWED_ORIGINS
```

예:

```text
GOEXCHANGE_CORS_ALLOWED_ORIGINS=http://13.125.10.20
GOEXCHANGE_WS_ALLOWED_ORIGINS=http://13.125.10.20
```

## 배포 실행

```bash
cd /opt/goexchange

docker compose --env-file .env.prod -f docker-compose.prod.yml pull
docker compose --env-file .env.prod -f docker-compose.prod.yml up -d
```

상태 확인:

```bash
docker compose --env-file .env.prod -f docker-compose.prod.yml ps
```

로그 확인:

```bash
docker compose --env-file .env.prod -f docker-compose.prod.yml logs -f backend
docker compose --env-file .env.prod -f docker-compose.prod.yml logs -f frontend
```

Health check:

```bash
curl http://localhost:8080/ping
curl http://localhost/healthz
```

브라우저 확인:

```text
http://<EC2_PUBLIC_IP>
```

## 새 버전 반영

GitHub Actions가 새 이미지를 GHCR에 push한 뒤 EC2에서 실행합니다.

```bash
cd /opt/goexchange

docker compose --env-file .env.prod -f docker-compose.prod.yml pull
docker compose --env-file .env.prod -f docker-compose.prod.yml up -d
```

## 롤백

`latest` 대신 commit SHA tag를 사용하면 특정 버전으로 되돌릴 수 있습니다.

`.env.prod`:

```text
BACKEND_IMAGE=ghcr.io/go-exchange-project/go-exchange-back:<OLD_COMMIT_SHA>
FRONTEND_IMAGE=ghcr.io/go-exchange-project/go-exchange-front:<OLD_COMMIT_SHA>
```

적용:

```bash
docker compose --env-file .env.prod -f docker-compose.prod.yml pull
docker compose --env-file .env.prod -f docker-compose.prod.yml up -d
```

## 다음 단계: GitHub Actions CD

수동 배포가 성공하면 GitHub Actions에서 아래 작업을 자동화합니다.

```text
main push
→ CI 성공
→ GHCR image push
→ EC2 SSH 접속
→ docker compose pull
→ docker compose up -d
→ /ping, /healthz 확인
```

GitHub Secrets 후보:

```text
EC2_HOST
EC2_USER
EC2_SSH_KEY
EC2_APP_DIR
```

GHCR package가 private이면 추가로 필요합니다.

```text
GHCR_USERNAME
GHCR_READ_TOKEN
```

## 자주 나는 문제

### 프론트에서 API 호출이 실패함

프론트 이미지가 잘못된 `VITE_API_BASE_URL`로 빌드됐을 가능성이 큽니다.

프론트 저장소 GitHub Variables를 확인합니다.

```text
VITE_API_BASE_URL=http://<EC2_PUBLIC_IP>:8080
VITE_WS_URL=ws://<EC2_PUBLIC_IP>:8080/ws
```

변경 후 프론트 저장소에 새 commit을 push해서 이미지를 다시 빌드합니다.

### CORS 또는 WebSocket origin 에러

백엔드 `.env.prod`의 origin 값이 브라우저 주소와 정확히 같아야 합니다.

```text
브라우저 주소: http://13.125.10.20
GOEXCHANGE_CORS_ALLOWED_ORIGINS=http://13.125.10.20
GOEXCHANGE_WS_ALLOWED_ORIGINS=http://13.125.10.20
```

### GHCR pull denied

GHCR package가 private인데 EC2에서 로그인하지 않은 상태입니다.

```bash
echo "<GHCR_READ_TOKEN>" | docker login ghcr.io -u "<GITHUB_USERNAME>" --password-stdin
```

### DB가 외부에서 접속되지 않음

정상입니다. PostgreSQL은 Docker 내부 network에서만 접근하게 두는 것이 맞습니다.
