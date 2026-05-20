package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	EnvJWTSecret     = "GOEXCHANGE_JWT_SECRET"
	UserIDContextKey = "userID"
)

var (
	ErrMissingToken     = errors.New("missing bearer token")
	ErrInvalidToken     = errors.New("invalid token")
	ErrExpiredToken     = errors.New("expired token")
	ErrInvalidJWTSecret = errors.New("jwt secret is required")
)

type TokenManager struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time
}

type tokenHeader struct {
	Algorithm string `json:"alg"`
	Type      string `json:"typ"`
}

type Claims struct {
	Subject   string `json:"sub"`
	ExpiresAt int64  `json:"exp"`
	IssuedAt  int64  `json:"iat"`
}

func NewTokenManager(secret string, ttl time.Duration) (*TokenManager, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return nil, ErrInvalidJWTSecret
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &TokenManager{
		secret: []byte(secret),
		ttl:    ttl,
		now:    time.Now,
	}, nil
}

func NewTokenManagerFromEnv() (*TokenManager, error) {
	secret := os.Getenv(EnvJWTSecret)
	if strings.TrimSpace(secret) == "" {
		secret = "dev-only-change-me"
	}
	return NewTokenManager(secret, 24*time.Hour)
}

func (tm *TokenManager) Generate(userID uint) (string, error) {
	if tm == nil {
		return "", ErrInvalidJWTSecret
	}
	if userID == 0 {
		return "", fmt.Errorf("user_id is required")
	}

	now := tm.now().UTC()
	header := tokenHeader{Algorithm: "HS256", Type: "JWT"}
	claims := Claims{
		Subject:   strconv.FormatUint(uint64(userID), 10),
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(tm.ttl).Unix(),
	}

	encodedHeader, err := encodeJSONSegment(header)
	if err != nil {
		return "", err
	}
	encodedClaims, err := encodeJSONSegment(claims)
	if err != nil {
		return "", err
	}

	signingInput := encodedHeader + "." + encodedClaims
	signature := tm.sign(signingInput)
	return signingInput + "." + signature, nil
}

func (tm *TokenManager) Parse(token string) (uint, error) {
	if tm == nil {
		return 0, ErrInvalidJWTSecret
	}

	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return 0, ErrInvalidToken
	}

	signingInput := parts[0] + "." + parts[1]
	expectedSignature := tm.sign(signingInput)
	if !hmac.Equal([]byte(expectedSignature), []byte(parts[2])) {
		return 0, ErrInvalidToken
	}

	var header tokenHeader
	if err := decodeJSONSegment(parts[0], &header); err != nil {
		return 0, ErrInvalidToken
	}
	if header.Algorithm != "HS256" || header.Type != "JWT" {
		return 0, ErrInvalidToken
	}

	var claims Claims
	if err := decodeJSONSegment(parts[1], &claims); err != nil {
		return 0, ErrInvalidToken
	}
	if claims.ExpiresAt <= tm.now().UTC().Unix() {
		return 0, ErrExpiredToken
	}

	parsedUserID, err := strconv.ParseUint(claims.Subject, 10, 64)
	if err != nil || parsedUserID == 0 {
		return 0, ErrInvalidToken
	}
	return uint(parsedUserID), nil
}

func ExtractBearerToken(header string) (string, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return "", ErrMissingToken
	}

	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", ErrMissingToken
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	if token == "" {
		return "", ErrMissingToken
	}
	return token, nil
}

func (tm *TokenManager) sign(signingInput string) string {
	mac := hmac.New(sha256.New, tm.secret)
	mac.Write([]byte(signingInput))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func encodeJSONSegment(value interface{}) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeJSONSegment(segment string, target interface{}) error {
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}
