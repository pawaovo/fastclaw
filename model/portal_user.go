package model

import (
	"errors"
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/util"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

type PortalUser struct {
	ID           string    `json:"id" gorm:"primaryKey;type:varchar(36)"`
	Email        string    `json:"email" gorm:"type:varchar(255);uniqueIndex;not null"`
	Name         string    `json:"name" gorm:"type:varchar(255)"`
	AvatarURL    string    `json:"avatar_url" gorm:"type:varchar(500)"`
	Provider     string    `json:"provider" gorm:"type:varchar(50);not null;default:'google'"`
	ProviderID   string    `json:"provider_id" gorm:"type:varchar(255);index"`
	PasswordHash string    `json:"-" gorm:"type:varchar(255)"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
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

func CreateLocalPortalUser(email, name, password string) (*PortalUser, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	name = strings.TrimSpace(name)
	password = strings.TrimSpace(password)
	if email == "" || password == "" {
		return nil, errors.New("email and password are required")
	}
	if len(password) < 8 {
		return nil, errors.New("password must be at least 8 characters")
	}
	if name == "" {
		name = strings.Split(email, "@")[0]
	}

	if _, err := GetPortalUserByEmail(email); err == nil {
		return nil, errors.New("email already exists")
	} else if err != gorm.ErrRecordNotFound {
		return nil, err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}

	user := &PortalUser{
		Email:        email,
		Name:         name,
		Provider:     "local",
		ProviderID:   email,
		PasswordHash: string(hash),
	}
	if err := util.GetDB().Create(user).Error; err != nil {
		return nil, err
	}
	return user, nil
}

func AuthenticateLocalPortalUser(email, password string) (*PortalUser, error) {
	user, err := GetPortalUserByEmail(email)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(user.PasswordHash) == "" {
		return nil, errors.New("password login is not enabled for this account")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return nil, errors.New("invalid email or password")
	}
	if strings.TrimSpace(user.Provider) == "" {
		user.Provider = "local"
	}
	return user, nil
}
