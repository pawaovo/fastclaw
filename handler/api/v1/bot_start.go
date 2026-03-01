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

func StartBot(c echo.Context) error {
	bot := middleware.GetBotFromContext(c)
	if bot == nil {
		return util.Forbidden(c, "not authorized")
	}

	if bot.Status == model.BotStatusRunning {
		return util.BadRequest(c, "bot is already running")
	}

	ctx := context.Background()

	// Get bot config for deployment
	openclawConfig, _ := bot.GetOpenClawConfig()
	k8sConfig := convertToK8sConfig(bot, openclawConfig)

	endpoint, err := runtime.StartBot(ctx, bot, k8sConfig)
	if err != nil {
		return util.InternalError(c, "failed to start bot: "+err.Error())
	}

	// Update bot status
	if err := model.UpdateBotStatus(bot.ID, model.BotStatusRunning, endpoint); err != nil {
		return util.InternalError(c, "failed to update bot status")
	}

	// Write config file to pod (async, don't block the response)
	// Config is required for token auth
	if !runtime.IsDockerPoolMode() && k8sConfig.AccessToken != "" {
		go func() {
			// On start, only set default model if user hasn't configured one
			if err := k8s.WriteConfigToBot(context.Background(), bot.ID, k8sConfig, false); err != nil {
				// Log error but don't fail the request
				c.Logger().Errorf("failed to write config to bot: %v", err)
			}
		}()
	}

	bot.Status = model.BotStatusRunning
	bot.Endpoint = endpoint

	return util.Success(c, bot)
}
