package v1

import (
	"context"

	"github.com/fastclaw-ai/fastclaw/middleware"
	"github.com/fastclaw-ai/fastclaw/model"
	"github.com/fastclaw-ai/fastclaw/service/k8s"
	"github.com/fastclaw-ai/fastclaw/service/runtime"
	"github.com/fastclaw-ai/fastclaw/util"
	"github.com/labstack/echo/v4"
)

func ResetBotToken(c echo.Context) error {
	bot := middleware.GetBotFromContext(c)
	if bot == nil {
		return util.Forbidden(c, "not authorized")
	}

	// Reset the access token
	newToken, err := model.ResetBotAccessToken(bot.ID)
	if err != nil {
		return util.InternalError(c, "failed to reset access token")
	}

	// Update config in database with new token
	updatedBot, err := model.GetBotByID(bot.ID)
	if err != nil {
		return util.InternalError(c, "failed to reload bot")
	}

	openclawConfig, _ := updatedBot.GetOpenClawConfig()
	if openclawConfig == nil {
		openclawConfig = &model.OpenClawConfig{}
	}
	// Ensure gateway.auth.token is updated
	if openclawConfig.Gateway == nil {
		openclawConfig.Gateway = &model.GatewayConfig{}
	}
	if openclawConfig.Gateway.Auth == nil {
		openclawConfig.Gateway.Auth = &model.GatewayAuthConfig{}
	}
	openclawConfig.Gateway.Auth.Mode = "token"
	openclawConfig.Gateway.Auth.Token = newToken

	// Save updated config to database
	if err := updatedBot.SetOpenClawConfig(openclawConfig); err != nil {
		c.Logger().Warnf("failed to update config with new token: %v", err)
	} else if err := model.UpdateBot(updatedBot); err != nil {
		c.Logger().Warnf("failed to save config with new token: %v", err)
	}

	// If bot is running, update deployment with new token
	if bot.Status == model.BotStatusRunning && !runtime.IsDockerPoolMode() {
		ctx := context.Background()
		k8sConfig := convertToK8sConfig(updatedBot, openclawConfig)

		// Update deployment spec with new token (this updates the startup command)
		if err := k8s.UpdateDeploymentConfig(ctx, bot.ID, newToken, k8sConfig); err != nil {
			c.Logger().Warnf("failed to update deployment after token reset: %v", err)
		}
	}

	return util.Success(c, map[string]interface{}{
		"id":           bot.ID,
		"access_token": newToken,
		"access_url":   buildAccessURL(bot.Slug, newToken),
		"message":      "access token has been reset",
	})
}
