package service

import (
	"errors"

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
