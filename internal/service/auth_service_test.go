package service

import (
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/auth"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

type fakeAuthUserRepository struct {
	nextID      uint
	usersByID   map[uint]*model.User
	usersByMail map[string]*model.User
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

func TestValidateRegisterInput(t *testing.T) {
	_, _, err := validateRegisterInput(RegisterInput{Name: "", Email: "alice@example.com", Password: "password123"})
	require.Error(t, err)

	_, _, err = validateRegisterInput(RegisterInput{Name: "Alice", Email: "not-email", Password: "password123"})
	require.Error(t, err)

	_, _, err = validateRegisterInput(RegisterInput{Name: "Alice", Email: "alice@example.com", Password: "short"})
	require.Error(t, err)
}
