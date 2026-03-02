package v1

import (
	"context"

	"github.com/fastclaw-ai/fastclaw/middleware"
	"github.com/fastclaw-ai/fastclaw/model"
	"github.com/fastclaw-ai/fastclaw/service/runtime"
	"github.com/fastclaw-ai/fastclaw/util"
	"github.com/labstack/echo/v4"
)

func DeleteBot(c echo.Context) error {
	bot := middleware.GetBotFromContext(c)
	if bot == nil {
		return util.Forbidden(c, "not authorized")
	}

	ctx := context.Background()

	// Delete runtime resources if running
	if bot.Status == model.BotStatusRunning {
		if err := runtime.StopBot(ctx, bot); err != nil {
			return util.InternalError(c, "failed to stop bot runtime")
		}
	} else {
		// Release stale lease in docker_pool mode (best effort).
		_ = runtime.ReleaseBot(bot.ID)
	}

	// Delete from database
	if err := model.DeleteBot(bot.ID); err != nil {
		return util.InternalError(c, "failed to delete bot")
	}

	// TODO: Clean up NAS data directory

	return util.Success(c, map[string]string{"message": "bot deleted"})
}
