# pg_stat_statements 기반 Postgres CPU 병목 조사 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** DB 인스턴스의 Postgres에서 `pg_stat_statements` 확장을 활성화해, 13번 테스트에서 확인한 CPU 389% 포화의 원인이 어떤 쿼리인지 실측할 수 있게 한다.

**Architecture:** `docker-compose.db.yml`의 `postgres` 서비스에 `shared_preload_libraries=pg_stat_statements`를 로드하는 `command`를 추가한다. 확장 자체는 컨테이너 기동 후 `CREATE EXTENSION`으로 1회 활성화한다(배포 후 사용자와 직접 진행).

**Tech Stack:** Postgres `pg_stat_statements` 확장(공식 contrib 모듈, `postgres:18-alpine` 이미지에 기본 포함).

## Global Constraints

- `docker-compose.db.yml`만 수정한다 — `docker-compose.stress.yml`, Terraform 등은 건드리지 않는다.
- 실제 재배포, 확장 활성화(`CREATE EXTENSION`), 재부하 테스트, 쿼리 조사, 결과 문서화는 이 계획의 범위 밖이다.

---

### Task 1: `docker-compose.db.yml`에 `pg_stat_statements` 프리로드 추가

**Files:**
- Modify: `docker-compose.db.yml`

**Interfaces:**
- 없음 (단일 태스크, 설정 파일만 변경).

- [ ] **Step 1: `postgres` 서비스에 `command` 추가**

`docker-compose.db.yml`의 현재:

```yaml
  postgres:
    image: postgres:18-alpine
    container_name: goexchange-db-postgres
    environment:
      POSTGRES_DB: ${POSTGRES_DB:-goexchange}
      POSTGRES_USER: ${POSTGRES_USER:-goexchange}
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD:?POSTGRES_PASSWORD is required}
    volumes:
      - goexchange-db-postgres-data:/var/lib/postgresql
    ports:
      - "5432:5432"
```

다음과 같이 수정한다:

```yaml
  postgres:
    image: postgres:18-alpine
    container_name: goexchange-db-postgres
    command: ["postgres", "-c", "shared_preload_libraries=pg_stat_statements", "-c", "pg_stat_statements.track=all"]
    environment:
      POSTGRES_DB: ${POSTGRES_DB:-goexchange}
      POSTGRES_USER: ${POSTGRES_USER:-goexchange}
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD:?POSTGRES_PASSWORD is required}
    volumes:
      - goexchange-db-postgres-data:/var/lib/postgresql
    ports:
      - "5432:5432"
```

- [ ] **Step 2: 문법 검증**

```bash
docker compose -f docker-compose.db.yml --env-file .env.stress.example config >/dev/null
```

Expected: 에러 없이 종료.

- [ ] **Step 3: 커밋**

```bash
git add docker-compose.db.yml
git commit -m "$(cat <<'MSG'
feat(infra): DB 인스턴스에 pg_stat_statements 프리로드 추가

13번 테스트에서 확인한 Postgres CPU 389% 포화의 원인 쿼리를 실측으로
찾기 위해, shared_preload_libraries로 pg_stat_statements를 로드한다.
확장 활성화(CREATE EXTENSION)는 배포 후 별도로 진행한다.
MSG
)"
```
