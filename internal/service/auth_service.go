package service

import (
	"errors"
	"fmt"
	"net/mail"
	"strings"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/auth"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

const (
	minPasswordLength = 8
	maxNameLength     = 80
	maxEmailLength    = 255
)

type authUserRepository interface {
	Create(user *model.User) error
	FindByEmail(email string) (*model.User, error)
	FindByID(id uint) (*model.User, error)
}

type AuthService struct {
	UserRepository authUserRepository
	TokenManager   *auth.TokenManager
}

type RegisterInput struct {
	Name     string
	Email    string
	Password string
}

type LoginInput struct {
	Email    string
	Password string
}

type AuthResult struct {
	Token string
	User  model.User
}

func NewAuthService(userRepo *repository.UserRepository, tokenManager *auth.TokenManager) *AuthService {
	return &AuthService{
		UserRepository: userRepo,
		TokenManager:   tokenManager,
	}
}

func (s *AuthService) Register(input RegisterInput) (AuthResult, error) {
	if s == nil || s.UserRepository == nil {
		return AuthResult{}, fmt.Errorf("user repository is required")
	}
	if s.TokenManager == nil {
		return AuthResult{}, fmt.Errorf("token manager is required")
	}

	name, email, err := validateRegisterInput(input)
	if err != nil {
		return AuthResult{}, err
	}

	existing, err := s.UserRepository.FindByEmail(email)
	if err == nil && existing.ID != 0 {
		return AuthResult{}, NewConflictErrorf("email is already registered")
	}
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return AuthResult{}, err
	}

	passwordHash, err := bcrypt.GenerateFromPassword([]byte(input.Password), bcrypt.DefaultCost)
	if err != nil {
		return AuthResult{}, err
	}

	user := &model.User{
		Name:         name,
		Email:        email,
		PasswordHash: string(passwordHash),
	}
	if err := s.UserRepository.Create(user); err != nil {
		return AuthResult{}, err
	}

	token, err := s.TokenManager.Generate(user.ID)
	if err != nil {
		return AuthResult{}, err
	}
	return AuthResult{Token: token, User: publicUser(*user)}, nil
}

func (s *AuthService) Login(input LoginInput) (AuthResult, error) {
	if s == nil || s.UserRepository == nil {
		return AuthResult{}, fmt.Errorf("user repository is required")
	}
	if s.TokenManager == nil {
		return AuthResult{}, fmt.Errorf("token manager is required")
	}

	email, err := normalizeEmail(input.Email)
	if err != nil {
		return AuthResult{}, err
	}
	user, err := s.UserRepository.FindByEmail(email)
	if err != nil {
		return AuthResult{}, fmt.Errorf("invalid email or password")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(input.Password)); err != nil {
		return AuthResult{}, fmt.Errorf("invalid email or password")
	}

	token, err := s.TokenManager.Generate(user.ID)
	if err != nil {
		return AuthResult{}, err
	}
	return AuthResult{Token: token, User: publicUser(*user)}, nil
}

func validateRegisterInput(input RegisterInput) (string, string, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return "", "", NewValidationErrorf("name is required")
	}
	if len(name) > maxNameLength {
		return "", "", NewValidationErrorf("name must be at most %d characters", maxNameLength)
	}

	email, err := normalizeEmail(input.Email)
	if err != nil {
		return "", "", err
	}
	if len(input.Password) < minPasswordLength {
		return "", "", NewValidationErrorf("password must be at least %d characters", minPasswordLength)
	}
	return name, email, nil
}

func normalizeEmail(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return "", NewValidationErrorf("email is required")
	}
	if len(email) > maxEmailLength {
		return "", NewValidationErrorf("email must be at most %d characters", maxEmailLength)
	}
	if _, err := mail.ParseAddress(email); err != nil {
		return "", NewValidationErrorf("email is invalid")
	}
	return email, nil
}

func publicUser(user model.User) model.User {
	user.PasswordHash = ""
	return user
}
