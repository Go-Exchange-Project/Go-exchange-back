package service

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"

	"github.com/shopspring/decimal"
)

// OrderFingerprintVersion은 지문 계산 규칙의 버전이다. 지문에 들어가는 필드 목록이
// 바뀌면 올린다. 비교는 항상 레코드에 저장된 버전의 규칙으로 하므로, 서버가 올라가도
// 이전 버전으로 저장된 키의 재시도가 409가 되지 않는다.
const OrderFingerprintVersion = 1

type OrderFingerprintInput struct {
	UserID      uint
	CoinSymbol  string
	Side        string
	OrderType   string
	Price       decimal.Decimal
	Amount      decimal.Decimal
	QuoteAmount decimal.Decimal
}

// ComputeOrderFingerprint는 주문을 결정하는 값만 모아 해시한다.
//
// DTO를 통째로 직렬화하지 않는다 — 필드 추가·키 순서·JSON 표현 변경만으로 기존 키가
// 전부 깨진다. 필드는 명시적으로 나열하고, 각 값은 길이-prefix로 이어 붙여 경계를
// 모호하지 않게 만든다("BTC"+"SELL"과 "BTCS"+"ELL"이 같은 입력이 되면 서로 다른
// 요청이 같은 지문을 갖는다).
func ComputeOrderFingerprint(in OrderFingerprintInput, version int) (string, error) {
	if version != OrderFingerprintVersion {
		return "", fmt.Errorf("unsupported order fingerprint version %d", version)
	}

	hash := sha256.New()
	write := func(value string) {
		var prefix [4]byte
		binary.BigEndian.PutUint32(prefix[:], uint32(len(value)))
		hash.Write(prefix[:])
		hash.Write([]byte(value))
	}

	write(fmt.Sprintf("v%d", version))
	write(strconv.FormatUint(uint64(in.UserID), 10))
	write(in.CoinSymbol)
	write(in.Side)
	write(in.OrderType)
	// decimal은 JSON 숫자나 부동소수점이 아니라 정규화된 문자열로 넣는다.
	write(normalizedDecimalString(in.Price))
	write(normalizedDecimalString(in.Amount))
	write(normalizedDecimalString(in.QuoteAmount))

	return hex.EncodeToString(hash.Sum(nil)), nil
}

// normalizedDecimalString은 표현 차이를 없앤다. decimal.Decimal의 String()은
// 후행 0을 제거하므로 1.50과 1.5가 같은 문자열이 된다.
func normalizedDecimalString(value decimal.Decimal) string {
	return value.Truncate(18).String()
}
