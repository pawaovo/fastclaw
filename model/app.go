package model

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/util"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type App struct {
	ID                string    `json:"id" gorm:"primaryKey;type:varchar(36)"`
	Name              string    `json:"name" gorm:"type:varchar(255);not null"`
	URL               string    `json:"url,omitempty" gorm:"type:varchar(500)"`
	Description       string    `json:"description,omitempty" gorm:"type:text"`
	OwnerEmail        string    `json:"owner_email,omitempty" gorm:"type:varchar(255)"`
	APIToken          string    `json:"api_token" gorm:"type:varchar(64);uniqueIndex;not null"`
	BotDomainTemplate string    `json:"bot_domain_template,omitempty" gorm:"type:varchar(500)"`
	Status            string    `json:"status" gorm:"type:varchar(50);default:'active'"` // active, disabled
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

func (App) TableName() string {
	return "apps"
}

func (a *App) BeforeCreate(tx *gorm.DB) error {
	if a.ID == "" {
		a.ID = uuid.New().String()
	}
	if a.APIToken == "" {
		a.APIToken = generateAppToken(64)
	}
	if a.Status == "" {
		a.Status = "active"
	}
	return nil
}

// generateAppToken generates a cryptographically secure API token
func generateAppToken(length int) string {
	bytes := make([]byte, length)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)[:length]
}

// Database operations

func CreateApp(app *App) error {
	return util.GetDB().Create(app).Error
}

func GetAppByID(id string) (*App, error) {
	var app App
	if err := util.GetDB().Where("id = ?", id).First(&app).Error; err != nil {
		return nil, err
	}
	return &app, nil
}

func GetAppByName(name string) (*App, error) {
	var app App
	if err := util.GetDB().Where("LOWER(name) = ?", strings.ToLower(strings.TrimSpace(name))).First(&app).Error; err != nil {
		return nil, err
	}
	return &app, nil
}

func GetAppByAPIToken(token string) (*App, error) {
	var app App
	if err := util.GetDB().Where("api_token = ? AND status = ?", token, "active").First(&app).Error; err != nil {
		return nil, err
	}
	return &app, nil
}

func ListApps() ([]*App, error) {
	var apps []*App
	if err := util.GetDB().Order("created_at DESC").Find(&apps).Error; err != nil {
		return nil, err
	}
	return apps, nil
}

func UpdateApp(app *App) error {
	return util.GetDB().Save(app).Error
}

func DeleteApp(id string) error {
	return util.GetDB().Where("id = ?", id).Delete(&App{}).Error
}

func ResetAppAPIToken(id string) (string, error) {
	newToken := generateAppToken(64)
	err := util.GetDB().Model(&App{}).Where("id = ?", id).Updates(map[string]interface{}{
		"api_token":  newToken,
		"updated_at": time.Now(),
	}).Error
	if err != nil {
		return "", err
	}
	return newToken, nil
}

// AutoMigrateApp creates the apps table if it doesn't exist
func AutoMigrateApp() error {
	return util.GetDB().AutoMigrate(&App{})
}
