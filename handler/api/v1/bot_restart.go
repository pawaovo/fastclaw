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

func RestartBot(c echo.Context) error {
	bot := middleware.GetBotFromContext(c)
	if bot == nil {
		return util.Forbidden(c, "not authorized")
	}

	if bot.Status != model.BotStatusRunning {
		return util.BadRequest(c, "bot is not running")
	}

	ctx := context.Background()

	if runtime.IsDockerPoolMode() {
		ready, err := runtime.GetBotReady(ctx, bot)
		if err != nil {
			return util.InternalError(c, "failed to check bot runtime status")
		}
		if !ready {
			return util.InternalError(c, "bot endpoint is not ready")
		}
		return util.Success(c, bot)
	}

	// Restart deployment (triggers rolling update)
	if err := k8s.RestartDeployment(ctx, bot.ID); err != nil {
		return util.InternalError(c, "failed to restart deployment: "+err.Error())
	}

	// Sync config to pod after restart
	go func() {
		if err := k8s.SyncConfigToPod(context.Background(), bot.ID); err != nil {
			c.Logger().Errorf("failed to sync config to bot: %v", err)
		}
	}()

	return util.Success(c, bot)
}
