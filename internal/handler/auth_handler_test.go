package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/auth"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// fakeAuthHandlerUserRepository는 D12(비주문 HTTP 경로의 55P03·57014 → 503,
// 그 밖은 401 유지)를 실제 HTTP 요청으로 검증하기 위한 최소 fake다.
type fakeAuthHandlerUserRepository struct {
	findByEmailErr error
	user           *model.User
}

func (r *fakeAuthHandlerUserRepository) Create(*model.User) error { return nil }

func (r *fakeAuthHandlerUserRepository) FindByEmail(string) (*model.User, error) {
	if r.findByEmailErr != nil {
		return nil, r.findByEmailErr
	}
	if r.user != nil {
		return r.user, nil
	}
	return nil, gorm.ErrRecordNotFound
}

func (r *fakeAuthHandlerUserRepository) FindByID(uint) (*model.User, error) {
	return nil, gorm.ErrRecordNotFound
}

func newLoginHandlerRequest(t *testing.T, handler *AuthHandler, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Login(c)
	return recorder
}

func newAuthHandlerWithRepo(t *testing.T, repo *fakeAuthHandlerUserRepository) *AuthHandler {
	t.Helper()
	tokenManager, err := auth.NewTokenManager("test-secret", 0)
	require.NoError(t, err)
	authService := &service.AuthService{UserRepository: repo, TokenManager: tokenManager}
	return NewAuthHandler(authService)
}

// D12: FindByEmail이 wrapped 57014를 반환하면 로그인은 503이다(기존처럼
// "invalid credentials"로 숨기지 않는다).
func TestLoginHandlerMapsStatementTimeoutTo503(t *testing.T) {
	repo := &fakeAuthHandlerUserRepository{
		findByEmailErr: &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"},
	}
	handler := newAuthHandlerWithRepo(t, repo)

	recorder := newLoginHandlerRequest(t, handler, `{"email":"alice@example.com","password":"whatever"}`)

	assert.Equal(t, http.StatusServiceUnavailable, recorder.Code, "body=%s", recorder.Body.String())
}

// D12: FindByEmail이 wrapped 55P03을 반환해도 503이다.
func TestLoginHandlerMapsLockTimeoutTo503(t *testing.T) {
	repo := &fakeAuthHandlerUserRepository{
		findByEmailErr: &pgconn.PgError{Code: "55P03", Message: "canceling statement due to lock timeout"},
	}
	handler := newAuthHandlerWithRepo(t, repo)

	recorder := newLoginHandlerRequest(t, handler, `{"email":"alice@example.com","password":"whatever"}`)

	assert.Equal(t, http.StatusServiceUnavailable, recorder.Code, "body=%s", recorder.Body.String())
}

// 대조군: 사용자 없음은 여전히 401이다 — 인증 실패 계약을 깨면 안 된다.
func TestLoginHandlerKeeps401ForUnknownUser(t *testing.T) {
	repo := &fakeAuthHandlerUserRepository{}
	handler := newAuthHandlerWithRepo(t, repo)

	recorder := newLoginHandlerRequest(t, handler, `{"email":"nobody@example.com","password":"whatever"}`)

	assert.Equal(t, http.StatusUnauthorized, recorder.Code, "body=%s", recorder.Body.String())
}

// 대조군: 비밀번호가 틀려도 여전히 401이다.
func TestLoginHandlerKeeps401ForWrongPassword(t *testing.T) {
	repo := &fakeAuthHandlerUserRepository{
		user: &model.User{ID: 1, Email: "alice@example.com", PasswordHash: mustBcryptHash(t, "correct-password")},
	}
	handler := newAuthHandlerWithRepo(t, repo)

	recorder := newLoginHandlerRequest(t, handler, `{"email":"alice@example.com","password":"wrong-password"}`)

	assert.Equal(t, http.StatusUnauthorized, recorder.Code, "body=%s", recorder.Body.String())
}

func mustBcryptHash(t *testing.T, password string) string {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	require.NoError(t, err)
	return string(hash)
}
