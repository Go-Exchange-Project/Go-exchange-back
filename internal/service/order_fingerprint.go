package service

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"

	"github.com/shopspring/decimal"
)

// CurrentOrderFingerprintVersion은 새 레코드를 저장할 때 쓰는 버전이다. 지문에 들어가는
// 필드 목록이 바뀌면 올린다.
//
// 이 상수를 올려도 아래 computeOrderFingerprintV1은 그대로 남아야 한다. 비교는 항상
// 레코드에 저장된 버전의 알고리즘으로 하므로, 그래야 배포만으로 기존 키의 재시도가
// 다른 지문을 얻지 않는다.
const CurrentOrderFingerprintVersion = 1

type OrderFingerprintInput struct {
	UserID      uint
	CoinSymbol  string
	Side        string
	OrderType   string
	Price       decimal.Decimal
	Amount      decimal.Decimal
	QuoteAmount decimal.Decimal
}

// ComputeOrderFingerprint는 version이 지정한 알고리즘으로 지문을 계산한다.
func ComputeOrderFingerprint(in OrderFingerprintInput, version int) (string, error) {
	switch version {
	case 1:
		return computeOrderFingerprintV1(in), nil
	default:
		return "", fmt.Errorf("unsupported order fingerprint version %d", version)
	}
}

// computeOrderFingerprintV1은 주문을 결정하는 값만 모아 해시한다.
//
// DTO를 통째로 직렬화하지 않는다 — 필드 추가·키 순서·JSON 표현 변경만으로 기존 키가
// 전부 깨진다. 필드는 명시적으로 나열하고, 각 값은 길이-prefix로 이어 붙여 경계를
// 모호하지 않게 만든다("BTC"+"SELL"과 "BTCS"+"ELL"이 같은 입력이 되면 서로 다른
// 요청이 같은 지문을 갖는다).
func computeOrderFingerprintV1(in OrderFingerprintInput) string {
	hash := sha256.New()
	write := func(value string) {
		var prefix [4]byte
		binary.BigEndian.PutUint32(prefix[:], uint32(len(value)))
		hash.Write(prefix[:])
		hash.Write([]byte(value))
	}

	write("v1")
	write(strconv.FormatUint(uint64(in.UserID), 10))
	write(in.CoinSymbol)
	write(in.Side)
	write(in.OrderType)
	// decimal은 JSON 숫자나 부동소수점이 아니라 문자열로 넣는다. String()은 후행 0을
	// 제거하므로 100.50과 100.5가 같은 지문이 된다. 자릿수는 자르지 않는다 — 자르면
	// 그 아래 자리만 다른 주문이 같은 지문이 된다.
	write(in.Price.String())
	write(in.Amount.String())
	write(in.QuoteAmount.String())

	return hex.EncodeToString(hash.Sum(nil))
}
