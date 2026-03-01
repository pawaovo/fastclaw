package v1

import (
	"context"

	"github.com/fastclaw-ai/fastclaw/middleware"
	"github.com/fastclaw-ai/fastclaw/model"
	"github.com/fastclaw-ai/fastclaw/service/runtime"
	"github.com/fastclaw-ai/fastclaw/util"
	"github.com/labstack/echo/v4"
)

func StopBot(c echo.Context) error {
	bot := middleware.GetBotFromContext(c)
	if bot == nil {
		return util.Forbidden(c, "not authorized")
	}

	if bot.Status != model.BotStatusRunning {
		return util.BadRequest(c, "bot is not running")
	}

	ctx := context.Background()

	if err := runtime.StopBot(ctx, bot); err != nil {
		return util.InternalError(c, "failed to stop bot: "+err.Error())
	}

	// Update bot status
	if err := model.UpdateBotStatus(bot.ID, model.BotStatusStopped, ""); err != nil {
		return util.InternalError(c, "failed to update bot status")
	}

	bot.Status = model.BotStatusStopped
	bot.Endpoint = ""

	return util.Success(c, bot)
}
