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
	case pgCodeSerializationFailure, pgCodeDeadlockDetected, pgCodeLockNotAvailable:
		return pgErr.Code
	}
	return ""
}
