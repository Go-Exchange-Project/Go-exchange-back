package model

import "time"

type User struct {
	ID		  uint      `gorm:"primaryKey"`
	Name      string    `gorm:"not null"`
	CreatedAt time.Time
}