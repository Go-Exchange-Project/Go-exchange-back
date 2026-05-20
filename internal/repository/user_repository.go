package repository

import (
	"fmt"
	"strings"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"gorm.io/gorm"
)

type UserRepository struct {
	DB *gorm.DB
}

func NewUserRepository(db *gorm.DB) *UserRepository {
	return &UserRepository{DB: db}
}

func (r *UserRepository) Create(user *model.User) error {
	if r == nil || r.DB == nil {
		return fmt.Errorf("user repository DB is required")
	}
	return r.DB.Create(user).Error
}

func (r *UserRepository) FindByEmail(email string) (*model.User, error) {
	if r == nil || r.DB == nil {
		return nil, fmt.Errorf("user repository DB is required")
	}

	var user model.User
	err := r.DB.Where("lower(email) = ?", strings.ToLower(strings.TrimSpace(email))).First(&user).Error
	return &user, err
}

func (r *UserRepository) FindByID(id uint) (*model.User, error) {
	if r == nil || r.DB == nil {
		return nil, fmt.Errorf("user repository DB is required")
	}
	if id == 0 {
		return nil, fmt.Errorf("user id is required")
	}

	var user model.User
	err := r.DB.First(&user, id).Error
	return &user, err
}
