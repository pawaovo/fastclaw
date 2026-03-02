package model

import (
	"errors"
	"time"

	"github.com/fastclaw-ai/fastclaw/util"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type RuntimeAllocation struct {
	BotID      string     `json:"bot_id" gorm:"primaryKey;type:varchar(36)"`
	Endpoint   string     `json:"endpoint" gorm:"type:varchar(255);index;not null"`
	ReleasedAt *time.Time `json:"released_at,omitempty" gorm:"type:timestamp;index"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

func (RuntimeAllocation) TableName() string {
	return "runtime_allocations"
}

func AutoMigrateRuntimeAllocation() error {
	return util.GetDB().AutoMigrate(&RuntimeAllocation{})
}

func AcquireEndpointLease(botID string, endpoints []string) (string, error) {
	if len(endpoints) == 0 {
		return "", errors.New("no endpoint configured")
	}

	var leasedEndpoint string
	err := util.GetDB().Transaction(func(tx *gorm.DB) error {
		// Serialize pool allocation to avoid duplicate endpoint assignment.
		if err := tx.Exec("LOCK TABLE runtime_allocations IN EXCLUSIVE MODE").Error; err != nil {
			return err
		}

		var current RuntimeAllocation
		if err := tx.Where("bot_id = ? AND released_at IS NULL", botID).First(&current).Error; err == nil {
			leasedEndpoint = current.Endpoint
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		var rows []RuntimeAllocation
		if err := tx.
			Where("released_at IS NULL").
			Find(&rows).Error; err != nil {
			return err
		}

		used := make(map[string]struct{}, len(rows))
		for _, r := range rows {
			if r.BotID != botID && r.Endpoint != "" {
				used[r.Endpoint] = struct{}{}
			}
		}

		for _, ep := range endpoints {
			if _, ok := used[ep]; ok {
				continue
			}

			leasedEndpoint = ep
			lease := RuntimeAllocation{
				BotID:      botID,
				Endpoint:   ep,
				ReleasedAt: nil,
			}
			return tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "bot_id"}},
				DoUpdates: clause.Assignments(map[string]interface{}{
					"endpoint":    ep,
					"released_at": nil,
					"updated_at":  time.Now(),
				}),
			}).Create(&lease).Error
		}

		return errors.New("no free docker pool endpoint")
	})
	if err != nil {
		return "", err
	}
	return leasedEndpoint, nil
}

func ReleaseEndpointLease(botID string) error {
	now := time.Now()
	return util.GetDB().
		Model(&RuntimeAllocation{}).
		Where("bot_id = ? AND released_at IS NULL", botID).
		Updates(map[string]interface{}{
			"released_at": now,
			"updated_at":  now,
		}).Error
}

func CountActiveEndpointLeases() (int64, error) {
	var count int64
	err := util.GetDB().
		Model(&RuntimeAllocation{}).
		Where("released_at IS NULL").
		Count(&count).Error
	return count, err
}

func ListActiveEndpointLeases() ([]RuntimeAllocation, error) {
	var rows []RuntimeAllocation
	err := util.GetDB().
		Where("released_at IS NULL").
		Find(&rows).Error
	return rows, err
}
