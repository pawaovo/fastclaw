package v1

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/fastclaw-ai/fastclaw/model"
	"github.com/fastclaw-ai/fastclaw/service/k8s"
	"github.com/fastclaw-ai/fastclaw/util"
	"github.com/labstack/echo/v4"
	"github.com/spf13/viper"
)

type UpgradeBotsRequest struct {
	Image string `json:"image"` // Target image, defaults to config value if empty
}

type UpgradeResult struct {
	BotID   string `json:"bot_id"`
	Status  string `json:"status"` // "upgraded", "failed", "skipped"
	Message string `json:"message,omitempty"`
}

type UpgradeBotsResponse struct {
	Image    string          `json:"image"`
	Total    int             `json:"total"`
	Upgraded int64           `json:"upgraded"`
	Failed   int64           `json:"failed"`
	Skipped  int64           `json:"skipped"`
	Results  []UpgradeResult `json:"results"`
}

// UpgradeBot upgrades a single bot's openclaw image
// POST /bot/api/v1/admin/bots/:id/upgrade
func UpgradeBot(c echo.Context) error {
	if err := ensureK8sMode(c); err != nil {
		return err
	}

	botID := c.Param("id")
	if botID == "" {
		return util.BadRequest(c, "bot id is required")
	}

	bot, err := model.GetBotByID(botID)
	if err != nil {
		return util.NotFound(c, "bot not found")
	}

	if bot.Status != model.BotStatusRunning {
		return util.BadRequest(c, "bot is not running")
	}

	var req UpgradeBotsRequest
	if err := c.Bind(&req); err != nil {
		return util.BadRequest(c, "invalid request body")
	}

	image := req.Image
	if image == "" {
		image = viper.GetString("openclaw.image")
	}
	if image == "" {
		return util.BadRequest(c, "no image specified")
	}

	ctx := context.Background()

	// Check current image
	currentImage, _ := k8s.GetDeploymentImage(ctx, bot.ID)
	if currentImage == image {
		return util.Success(c, map[string]string{
			"status":  "skipped",
			"message": "already running target image",
			"image":   image,
		})
	}

	if err := k8s.UpdateDeploymentImage(ctx, bot.ID, image); err != nil {
		return util.InternalError(c, "failed to upgrade: "+err.Error())
	}

	// Sync config to new pod after image upgrade (applies latest gateway settings)
	go func() {
		if err := k8s.SyncConfigToPod(context.Background(), bot.ID); err != nil {
			fmt.Printf("[Upgrade] failed to sync config for bot %s: %v\n", bot.ID, err)
		}
	}()

	return util.Success(c, map[string]string{
		"status":         "upgraded",
		"image":          image,
		"previous_image": currentImage,
	})
}

// UpgradeAllBots upgrades all running bots to a new openclaw image
// POST /bot/api/v1/admin/bots/upgrade
func UpgradeAllBots(c echo.Context) error {
	if err := ensureK8sMode(c); err != nil {
		return err
	}

	var req UpgradeBotsRequest
	if err := c.Bind(&req); err != nil {
		return util.BadRequest(c, "invalid request body")
	}

	image := req.Image
	if image == "" {
		image = viper.GetString("openclaw.image")
	}
	if image == "" {
		return util.BadRequest(c, "no image specified")
	}

	bots, err := model.ListBotsByStatus(model.BotStatusRunning)
	if err != nil {
		return util.InternalError(c, "failed to list running bots")
	}

	if len(bots) == 0 {
		return util.Success(c, &UpgradeBotsResponse{
			Image: image,
			Total: 0,
		})
	}

	ctx := context.Background()
	var upgraded, failed, skipped atomic.Int64
	results := make([]UpgradeResult, len(bots))

	// Upgrade concurrently with limited parallelism
	var wg sync.WaitGroup
	sem := make(chan struct{}, 10) // max 10 concurrent upgrades

	for i, bot := range bots {
		wg.Add(1)
		go func(idx int, b *model.Bot) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			result := UpgradeResult{BotID: b.ID}

			// Check current image
			currentImage, err := k8s.GetDeploymentImage(ctx, b.ID)
			if err != nil {
				// Deployment might not exist, skip
				skipped.Add(1)
				result.Status = "skipped"
				result.Message = fmt.Sprintf("deployment not found: %v", err)
				results[idx] = result
				return
			}

			if currentImage == image {
				skipped.Add(1)
				result.Status = "skipped"
				result.Message = "already running target image"
				results[idx] = result
				return
			}

			if err := k8s.UpdateDeploymentImage(ctx, b.ID, image); err != nil {
				failed.Add(1)
				result.Status = "failed"
				result.Message = err.Error()
			} else {
				upgraded.Add(1)
				result.Status = "upgraded"
				// Sync config to new pod after image upgrade
				go func(botID string) {
					if err := k8s.SyncConfigToPod(context.Background(), botID); err != nil {
						fmt.Printf("[Upgrade] failed to sync config for bot %s: %v\n", botID, err)
					}
				}(b.ID)
			}
			results[idx] = result
		}(i, bot)
	}

	wg.Wait()

	return util.Success(c, &UpgradeBotsResponse{
		Image:    image,
		Total:    len(bots),
		Upgraded: upgraded.Load(),
		Failed:   failed.Load(),
		Skipped:  skipped.Load(),
		Results:  results,
	})
}
