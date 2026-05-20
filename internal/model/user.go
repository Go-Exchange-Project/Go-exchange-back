package model

import "time"

type User struct {
	ID           uint   `gorm:"primaryKey"`
	Name         string `gorm:"not null"`
	Email        string `gorm:"size:255"`
	PasswordHash string `gorm:"size:255"`
	CreatedAt    time.Time
}
