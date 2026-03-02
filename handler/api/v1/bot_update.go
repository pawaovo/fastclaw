package v1

import (
	"context"
	"time"

	"github.com/fastclaw-ai/fastclaw/middleware"
	"github.com/fastclaw-ai/fastclaw/model"
	"github.com/fastclaw-ai/fastclaw/service/k8s"
	"github.com/fastclaw-ai/fastclaw/service/runtime"
	"github.com/fastclaw-ai/fastclaw/util"
	"github.com/labstack/echo/v4"
)

type UpdateBotRequest struct {
	Name      string                 `json:"name,omitempty"`
	Slug      string                 `json:"slug,omitempty"`
	Config    map[string]interface{} `json:"config,omitempty"` // OpenClaw native config format
	ExpiresAt *time.Time             `json:"expires_at,omitempty"`
}

func UpdateBot(c echo.Context) error {
	bot := middleware.GetBotFromContext(c)
	if bot == nil {
		return util.Forbidden(c, "not authorized")
	}

	var req UpdateBotRequest
	if err := c.Bind(&req); err != nil {
		return util.BadRequest(c, "invalid request body")
	}

	if req.Name != "" {
		bot.Name = req.Name
	}

	if req.Slug != "" {
		if !isValidSlug(req.Slug) {
			return util.BadRequest(c, "slug must be 1-50 characters, lowercase letters, numbers, and hyphens only")
		}
		// Check if slug is already taken by another bot
		existing, _ := model.GetBotBySlug(req.Slug)
		if existing != nil && existing.ID != bot.ID {
			return util.BadRequest(c, "slug is already taken")
		}
		bot.Slug = req.Slug
	}

	// Update config if provided (OpenClaw native format)
	if req.Config != nil {
		if err := bot.SetConfigMap(req.Config); err != nil {
			return util.InternalError(c, "failed to set config")
		}
	}

	// Update expiration time (for renewal)
	if req.ExpiresAt != nil {
		bot.ExpiresAt = req.ExpiresAt
	}

	if err := model.UpdateBot(bot); err != nil {
		return util.InternalError(c, "failed to update bot")
	}

	// If bot is running and config was updated, sync to pod
	if req.Config != nil && bot.Status == model.BotStatusRunning && !runtime.IsDockerPoolMode() {
		go func() {
			ctx := context.Background()
			if err := k8s.SyncConfigToPod(ctx, bot.ID); err != nil {
				c.Logger().Errorf("failed to sync config to pod: %v", err)
			}
		}()
	}

	return util.Success(c, bot)
}
