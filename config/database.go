package config

import (
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"log"
)

var DB *gorm.DB

func ConnectDB(){
	dsn := "host=localhost user=postgres password=1234 dbname=goexchange port=5432 sslmode=disable"

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatal("DB 연결 실패: ", err)
	}

	DB = db
	log.Println("DB 연결 성공!")
}