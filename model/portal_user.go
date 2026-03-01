package model

import (
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/util"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type PortalUser struct {
	ID         string    `json:"id" gorm:"primaryKey;type:varchar(36)"`
	Email      string    `json:"email" gorm:"type:varchar(255);uniqueIndex;not null"`
	Name       string    `json:"name" gorm:"type:varchar(255)"`
	AvatarURL  string    `json:"avatar_url" gorm:"type:varchar(500)"`
	Provider   string    `json:"provider" gorm:"type:varchar(50);not null;default:'google'"`
	ProviderID string    `json:"provider_id" gorm:"type:varchar(255);index"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func (PortalUser) TableName() string {
	return "portal_users"
}

func (u *PortalUser) BeforeCreate(tx *gorm.DB) error {
	if u.ID == "" {
		u.ID = uuid.New().String()
	}
	if u.Provider == "" {
		u.Provider = "google"
	}
	u.Email = strings.TrimSpace(strings.ToLower(u.Email))
	return nil
}

func AutoMigratePortalUser() error {
	return util.GetDB().AutoMigrate(&PortalUser{})
}

func GetPortalUserByID(id string) (*PortalUser, error) {
	var user PortalUser
	if err := util.GetDB().Where("id = ?", id).First(&user).Error; err != nil {
		return nil, err
	}
	return &user, nil
}

func GetPortalUserByEmail(email string) (*PortalUser, error) {
	var user PortalUser
	if err := util.GetDB().Where("email = ?", strings.TrimSpace(strings.ToLower(email))).First(&user).Error; err != nil {
		return nil, err
	}
	return &user, nil
}

func UpsertGooglePortalUser(email, name, avatarURL, providerID string) (*PortalUser, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	user, err := GetPortalUserByEmail(email)
	if err != nil {
		if err != gorm.ErrRecordNotFound {
			return nil, err
		}
		user = &PortalUser{
			Email:      email,
			Name:       name,
			AvatarURL:  avatarURL,
			Provider:   "google",
			ProviderID: providerID,
		}
		if err := util.GetDB().Create(user).Error; err != nil {
			return nil, err
		}
		return user, nil
	}

	user.Name = name
	user.AvatarURL = avatarURL
	user.Provider = "google"
	user.ProviderID = providerID
	if err := util.GetDB().Save(user).Error; err != nil {
		return nil, err
	}
	return user, nil
}
