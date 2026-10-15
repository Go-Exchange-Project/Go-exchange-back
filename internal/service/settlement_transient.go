package service

import (
	"errors"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/metrics"
	"github.com/jackc/pgx/v5/pgconn"
)

// Postgres SQLSTATE 코드 중 재시도로 해소되는 일시적 오류.
const (
	pgCodeSerializationFailure = "40001"
	pgCodeDeadlockDetected     = "40P01"
	pgCodeLockNotAvailable     = "55P03"
	// pgCodeQueryCanceled(57014)는 일반적인 query_canceled 코드라 수동 취소에도
	// 쓰인다. 지금은 요청 context 취소가 없어 항상 statement_timeout이 원인이므로
	// transient가 맞지만, 후속 context 전파 작업에서는 context.Canceled와 구분해야
	// 한다(설계 §4.2, 계획 Task 2 Step 2 주석).
	pgCodeQueryCanceled = "57014"
)

// IsTransientSettlementError는 재시도하면 성공할 수 있는 DB 오류인지 판정합니다.
// 에러 메시지 문자열은 lc_messages 설정에 따라 번역될 수 있으므로 SQLSTATE로만 판정합니다.
func IsTransientSettlementError(err error) bool {
	return settlementErrorSQLState(err) != ""
}

// settlementErrorSQLState는 err 체인에서 transient SQLSTATE 코드를 찾아 반환합니다.
// transient가 아니거나 Postgres 오류가 아니면 빈 문자열을 반환합니다.
func settlementErrorSQLState(err error) string {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return ""
	}
	switch pgErr.Code {
	case pgCodeSerializationFailure, pgCodeDeadlockDetected, pgCodeLockNotAvailable, pgCodeQueryCanceled:
		return pgErr.Code
	}
	return ""
}

// SettlementErrorSQLState는 settlementErrorSQLState를 패키지 밖에 노출한다.
// cmd/main.go의 goexchange_db_timeout_total{sqlstate,path} 계측처럼, transient
// 여부(bool)가 아니라 실제 SQLSTATE 값 자체가 필요한 호출자를 위한 것이다.
func SettlementErrorSQLState(err error) string {
	return settlementErrorSQLState(err)
}

// recordDBTimeoutMetric은 cmd/main.go의 동명 헬퍼와 같은 계약이다 — 55P03
// (lock_timeout)·57014(statement_timeout) 두 SQLSTATE만 경로 라벨과 함께
// 센다(설계 §4.2·§3.3의 "배포 전 55P03·57014 발생 건수 측정"). deadlock 등
// 다른 transient 오류는 "DB 시간 상한"이 아니므로 포함하지 않는다. hold
// coordinator·retry worker의 배치·재시도 경로에서, 제어 흐름(단건 폴백 여부)과
// 무관하게 계측만 이 함수로 남긴다.
func recordDBTimeoutMetric(err error, path string) {
	switch code := settlementErrorSQLState(err); code {
	case pgCodeLockNotAvailable, pgCodeQueryCanceled:
		metrics.DBTimeoutTotal.WithLabelValues(code, path).Inc()
	}
}
