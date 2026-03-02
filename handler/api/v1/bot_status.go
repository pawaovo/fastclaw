package v1

import (
	"context"

	"github.com/fastclaw-ai/fastclaw/middleware"
	"github.com/fastclaw-ai/fastclaw/model"
	"github.com/fastclaw-ai/fastclaw/service/runtime"
	"github.com/fastclaw-ai/fastclaw/util"
	"github.com/labstack/echo/v4"
)

type BotStatusResponse struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Status   model.BotStatus `json:"status"`
	Ready    bool            `json:"ready"`
	Endpoint string          `json:"endpoint,omitempty"`
}

func GetBotStatus(c echo.Context) error {
	bot := middleware.GetBotFromContext(c)
	if bot == nil {
		return util.Forbidden(c, "not authorized")
	}

	response := BotStatusResponse{
		ID:       bot.ID,
		Name:     bot.Name,
		Status:   bot.Status,
		Endpoint: bot.Endpoint,
	}

	// Check actual K8s status if bot is supposed to be running
	if bot.Status == model.BotStatusRunning {
		ctx := context.Background()
		ready, err := runtime.GetBotReady(ctx, bot)
		if err != nil {
			response.Ready = false
		} else {
			response.Ready = ready
			// Sync status: if runtime workload doesn't exist, update DB to stopped
			if !ready {
				exists, _ := runtime.DeploymentExists(ctx, bot.ID)
				if !exists {
					bot.Status = model.BotStatusStopped
					bot.Endpoint = ""
					model.UpdateBot(bot)
					response.Status = model.BotStatusStopped
					response.Endpoint = ""
				}
			}
		}
	}

	return util.Success(c, response)
}
