package service

import (
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/auth"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

type fakeAuthUserRepository struct {
	nextID      uint
	usersByID   map[uint]*model.User
	usersByMail map[string]*model.User
	// findByEmailErr가 설정되면 FindByEmail이 맵 조회 대신 이 오류를 돌려준다
	// (D12: FindByEmail이 wrapped 55P03·57014를 반환하는 경우를 재현한다).
	findByEmailErr error
}

func newFakeAuthUserRepository() *fakeAuthUserRepository {
	return &fakeAuthUserRepository{
		nextID:      1,
		usersByID:   make(map[uint]*model.User),
		usersByMail: make(map[string]*model.User),
	}
}

func (r *fakeAuthUserRepository) Create(user *model.User) error {
	user.ID = r.nextID
	r.nextID++
	copied := *user
	r.usersByID[user.ID] = &copied
	r.usersByMail[user.Email] = &copied
	return nil
}

func (r *fakeAuthUserRepository) FindByEmail(email string) (*model.User, error) {
	if r.findByEmailErr != nil {
		return nil, r.findByEmailErr
	}
	user, ok := r.usersByMail[email]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	copied := *user
	return &copied, nil
}

func (r *fakeAuthUserRepository) FindByID(id uint) (*model.User, error) {
	user, ok := r.usersByID[id]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	copied := *user
	return &copied, nil
}

func TestAuthServiceRegisterCreatesUserAndToken(t *testing.T) {
	repo := newFakeAuthUserRepository()
	tokenManager, err := auth.NewTokenManager("test-secret", time.Hour)
	require.NoError(t, err)
	service := &AuthService{UserRepository: repo, TokenManager: tokenManager}

	result, err := service.Register(RegisterInput{
		Name:     "  Alice  ",
		Email:    " ALICE@example.com ",
		Password: "password123",
	})

	require.NoError(t, err)
	assert.NotEmpty(t, result.Token)
	assert.Equal(t, uint(1), result.User.ID)
	assert.Equal(t, "Alice", result.User.Name)
	assert.Equal(t, "alice@example.com", result.User.Email)
	assert.Empty(t, result.User.PasswordHash)

	persisted := repo.usersByMail["alice@example.com"]
	require.NotNil(t, persisted)
	assert.NoError(t, bcrypt.CompareHashAndPassword([]byte(persisted.PasswordHash), []byte("password123")))
}

func TestAuthServiceRegisterRejectsDuplicateEmail(t *testing.T) {
	repo := newFakeAuthUserRepository()
	tokenManager, err := auth.NewTokenManager("test-secret", time.Hour)
	require.NoError(t, err)
	service := &AuthService{UserRepository: repo, TokenManager: tokenManager}

	_, err = service.Register(RegisterInput{Name: "Alice", Email: "alice@example.com", Password: "password123"})
	require.NoError(t, err)

	_, err = service.Register(RegisterInput{Name: "Alice2", Email: "alice@example.com", Password: "password123"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already registered")
}

func TestAuthServiceLoginValidatesPasswordAndReturnsToken(t *testing.T) {
	repo := newFakeAuthUserRepository()
	tokenManager, err := auth.NewTokenManager("test-secret", time.Hour)
	require.NoError(t, err)
	service := &AuthService{UserRepository: repo, TokenManager: tokenManager}
	_, err = service.Register(RegisterInput{Name: "Alice", Email: "alice@example.com", Password: "password123"})
	require.NoError(t, err)

	result, err := service.Login(LoginInput{Email: "alice@example.com", Password: "password123"})

	require.NoError(t, err)
	assert.NotEmpty(t, result.Token)
	assert.Equal(t, "alice@example.com", result.User.Email)
}

func TestAuthServiceLoginRejectsWrongPassword(t *testing.T) {
	repo := newFakeAuthUserRepository()
	tokenManager, err := auth.NewTokenManager("test-secret", time.Hour)
	require.NoError(t, err)
	service := &AuthService{UserRepository: repo, TokenManager: tokenManager}
	_, err = service.Register(RegisterInput{Name: "Alice", Email: "alice@example.com", Password: "password123"})
	require.NoError(t, err)

	_, err = service.Login(LoginInput{Email: "alice@example.com", Password: "wrong-password"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid email or password")
}

// D12: FindByEmail이 wrapped 55P03·57014를 반환하면 Login은 일반 "invalid email
// or password"가 아니라 ErrorKindUnavailable로 구분되는 오류를 돌려줘야 한다 —
// 그래야 핸들러가 503으로 매핑하고, "재시도하면 될 수도 있다"는 인증 실패와
// 섞이지 않는다.
func TestAuthServiceLoginReturnsUnavailableOnStatementTimeout(t *testing.T) {
	repo := newFakeAuthUserRepository()
	repo.findByEmailErr = &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"}
	tokenManager, err := auth.NewTokenManager("test-secret", time.Hour)
	require.NoError(t, err)
	service := &AuthService{UserRepository: repo, TokenManager: tokenManager}

	_, err = service.Login(LoginInput{Email: "alice@example.com", Password: "whatever"})
	require.Error(t, err)
	kind, ok := DomainErrorKind(err)
	require.True(t, ok, "57014는 DomainError(Unavailable)로 구분돼야 한다: %v", err)
	assert.Equal(t, ErrorKindUnavailable, kind)
}

func TestAuthServiceLoginReturnsUnavailableOnLockTimeout(t *testing.T) {
	repo := newFakeAuthUserRepository()
	repo.findByEmailErr = &pgconn.PgError{Code: "55P03", Message: "canceling statement due to lock timeout"}
	tokenManager, err := auth.NewTokenManager("test-secret", time.Hour)
	require.NoError(t, err)
	service := &AuthService{UserRepository: repo, TokenManager: tokenManager}

	_, err = service.Login(LoginInput{Email: "alice@example.com", Password: "whatever"})
	require.Error(t, err)
	kind, ok := DomainErrorKind(err)
	require.True(t, ok, "55P03도 DomainError(Unavailable)로 구분돼야 한다: %v", err)
	assert.Equal(t, ErrorKindUnavailable, kind)
}

// 대조군: 사용자 없음(gorm.ErrRecordNotFound)은 여전히 일반 오류다 — 401 계약을
// 깨면 안 된다(DomainError가 아니어야 핸들러가 기존 401 분기를 그대로 탄다).
func TestAuthServiceLoginKeepsGenericErrorForUnknownUser(t *testing.T) {
	repo := newFakeAuthUserRepository()
	tokenManager, err := auth.NewTokenManager("test-secret", time.Hour)
	require.NoError(t, err)
	service := &AuthService{UserRepository: repo, TokenManager: tokenManager}

	_, err = service.Login(LoginInput{Email: "nobody@example.com", Password: "whatever"})
	require.Error(t, err)
	_, ok := DomainErrorKind(err)
	assert.False(t, ok, "사용자 없음은 DomainError가 아니어야 401 그대로 유지된다")
	assert.Contains(t, err.Error(), "invalid email or password")
}

func TestValidateRegisterInput(t *testing.T) {
	_, _, err := validateRegisterInput(RegisterInput{Name: "", Email: "alice@example.com", Password: "password123"})
	require.Error(t, err)

	_, _, err = validateRegisterInput(RegisterInput{Name: "Alice", Email: "not-email", Password: "password123"})
	require.Error(t, err)

	_, _, err = validateRegisterInput(RegisterInput{Name: "Alice", Email: "alice@example.com", Password: "short"})
	require.Error(t, err)
}
