package v1

import (
	"context"

	"github.com/fastclaw-ai/fastclaw/middleware"
	"github.com/fastclaw-ai/fastclaw/model"
	"github.com/fastclaw-ai/fastclaw/service/k8s"
	"github.com/fastclaw-ai/fastclaw/service/runtime"
	"github.com/fastclaw-ai/fastclaw/util"
	"github.com/labstack/echo/v4"
	"github.com/spf13/viper"
)

// GetBotResponse includes bot info and deployment status
type GetBotResponse struct {
	*model.Bot
	DeploymentStatus *k8s.DeploymentStatusInfo `json:"deployment_status,omitempty"`
	Image            string                    `json:"image,omitempty"`
	LatestImage      string                    `json:"latest_image,omitempty"`
	ImageUpToDate    *bool                     `json:"image_up_to_date,omitempty"`
}

func GetBot(c echo.Context) error {
	bot := middleware.GetBotFromContext(c)
	if bot == nil {
		return util.Forbidden(c, "not authorized")
	}

	ctx := context.Background()
	response := &GetBotResponse{Bot: bot}

	// If bot is running, get deployment status, image info, and sync config
	if bot.Status == model.BotStatusRunning && !runtime.IsDockerPoolMode() {
		// Get deployment status
		if statusInfo, err := k8s.GetDeploymentStatusInfo(ctx, bot.ID); err == nil {
			response.DeploymentStatus = statusInfo
		}

		// Get current and latest image for upgrade check
		if currentImage, err := k8s.GetDeploymentImage(ctx, bot.ID); err == nil {
			response.Image = currentImage
			latestImage := viper.GetString("openclaw.image")
			if latestImage != "" {
				response.LatestImage = latestImage
				upToDate := currentImage == latestImage
				response.ImageUpToDate = &upToDate
			}
		}

		// Only sync config if deployment is ready (not during updates)
		if response.DeploymentStatus != nil && response.DeploymentStatus.Status == "ready" {
			if err := k8s.SyncConfigToDatabase(ctx, bot.ID); err != nil {
				c.Logger().Warnf("failed to sync config from pod: %v", err)
			} else {
				// Reload bot to get updated config
				if updatedBot, err := model.GetBotByID(bot.ID); err == nil {
					response.Bot = updatedBot
				}
			}
		}
	}

	return util.Success(c, response)
}
